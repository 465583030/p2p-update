package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/zeebo/bencode"
)

const (
	// UUIDApk is the UUID of updates that uses APK (Alpine Package Management)
	// for deployment.
	// Generated by invoking:
	// $ uuidgen --sha1 --namespace @oid --name /sbin/apk
	UUIDApk = "5ee3a38d-a8dc-514e-9c74-42ab160648aa"

	// UUIDShell is the UUID of updates that uses shell script for deployment.
	// Generated by invoking:
	// $ uuidgen --sha1 --namespace @oid --name /bin/sh
	UUIDShell = "f5adf0cb-b0e1-5a22-97f1-09092f566438"

	// DeployFailsLimit is the maximum fails of deployment. Exceeding this value
	// means that the update should not be deployed.
	DeployFailsLimit = 5

	// ShellExecutionTimeout is the maximum execution time of a shell script
	// before timeout.
	ShellExecutionTimeout = 600 // in seconds
)

// Update represents a system update that should be downloaded and deployed on
// the system. It also has to be distributed to other peers.
type Update struct {
	sync.RWMutex

	Metainfo Metainfo  `json:"metainfo"`
	Deployed time.Time `json:"deployed"`

	torrent          *torrent.Torrent
	stopped          bool
	sent             bool
	deployFails      int
	metadataFilename string
}

// NewUpdateFromMessage creates an Update instance from a byte-array of torrent++.
func NewUpdateFromMessage(b []byte, metadataDir string) (*Update, error) {
	u := Update{
		stopped: true,
		sent:    false,
	}
	if err := bencode.DecodeBytes(b, &u.Metainfo); err != nil {
		return nil, err
	}
	u.SetMetadataFilename(metadataDir)
	return &u, nil
}

// LoadUpdateFromFile loads Update description from given filename.
func LoadUpdateFromFile(filename string) (*Update, error) {
	u := Update{
		stopped:          true,
		sent:             false,
		metadataFilename: filename,
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	return &u, json.NewDecoder(f).Decode(&u)
}

// SetMetadataFilename sets the filename where the metadata of this update
// will be saved.
func (u *Update) SetMetadataFilename(metadataDir string) {
	u.metadataFilename = filepath.Join(metadataDir, fmt.Sprintf("%s-v%d",
		u.Metainfo.UUID, u.Metainfo.Version))
}

// Save writes Update metadata to file.
func (u *Update) Save() error {
	u.RLock()
	defer u.RUnlock()
	f, err := os.OpenFile(u.metadataFilename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		return err
	}
	return json.NewEncoder(f).Encode(u)
}

// Verify verifies the update. It returns an error if the verification fails,
// otherwise nil.
func (u *Update) Verify(a *Agent) error {
	if u.Metainfo.Verify(*a.PublicKey) != nil {
		return errUpdateVerificationFailed
	}
	return nil
}

// Start starts the update's lifecycle.
func (u *Update) Start(a *Agent) error {
	u.Lock()
	defer u.Unlock()

	var (
		mi  *metainfo.MetaInfo
		err error
	)

	if err = u.Verify(a); err != nil {
		return err
	}

	// Remove existing update that has the same UUID. If the existing update
	// is newer, then return an error.
	if cu, ok := a.Updates[u.Metainfo.UUID]; ok {
		if cu.Metainfo.Version > u.Metainfo.Version {
			return errUpdateIsOlder
		} else if cu.Metainfo.Version == u.Metainfo.Version {
			return errUpdateIsAlreadyExist
		}
		log.Printf("stopping older update of uuid:%s", u.Metainfo.UUID)
		cu.Stop()
		if err = cu.Delete(); err != nil {
			log.Printf("WARNING: failed to delete update uuid:%s version:%d : %v",
				cu.Metainfo.UUID, cu.Metainfo.Version, err)
		}
	} else {
		log.Printf("older update of uuid:%s does not exist", u.Metainfo.UUID)
	}
	a.Updates[u.Metainfo.UUID] = u

	// activate torrent
	log.Printf("starting update: %s", u.String())
	if mi, err = u.Metainfo.torrentMetainfo(); err != nil {
		return fmt.Errorf("failed generating torrent metainfo: %v", err)
	}
	if u.torrent, err = a.torrentClient.AddTorrent(mi); err != nil {
		return fmt.Errorf("failed adding torrent: %v", err)
	}
	u.stopped = false
	log.Printf("started update: %s", u.String())

	// spawn a go-routine that monitors torrent's status
	go u.monitor(a)

	return nil
}

func (u *Update) monitor(a *Agent) {
	for {
		time.Sleep(5 * time.Second)
		toSave := false

		u.Lock()
		if u.stopped {
			break
		}
		if !u.sent {
			if err := u.Send(a); err != nil {
				log.Printf("failed sending update uuid:%s version:%d : %v",
					u.Metainfo.UUID, u.Metainfo.Version, err)
			}
			u.sent = true
			toSave = true
		}
		if u.torrent.BytesMissing() > 0 {
			<-u.torrent.GotInfo()
			u.torrent.DownloadAll()
		} else if u.Deployed.Year() < 2000 {
			u.deploy()
			toSave = true
		}
		log.Println(u.String())
		u.Unlock()

		if toSave {
			u.Save()
		}
	}
}

// Stop stops the lifecycle of the update.
func (u *Update) Stop() {
	u.Lock()
	defer u.Unlock()
	if u.torrent != nil {
		log.Printf("stopping torrent: %v", u.String())
		u.torrent.Drop()
		<-u.torrent.Closed()
		u.stopped = true
		log.Printf("closed torrent: %v", u.String())
	}
}

// Delete deletes this update files.
func (u *Update) Delete() error {
	u.Lock()
	defer u.Unlock()
	if !u.stopped {
		return fmt.Errorf("update has not been stopped")
	}
	if u.torrent != nil {
		for _, f := range u.torrent.Files() {
			if _, err := os.Stat(f.Path()); err == nil {
				if err = os.Remove(f.Path()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Send sends the Update to the peers.
func (u *Update) Send(a *Agent) error {
	return u.Metainfo.Write(a.Overlay)
}

func (u *Update) String() string {
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("uuid:%v version:%d", u.Metainfo.UUID, u.Metainfo.Version))
	if u.torrent != nil {
		b.WriteString(fmt.Sprintf(" completed/missing:%v/%v",
			u.torrent.BytesCompleted(), u.torrent.BytesMissing()))
		stats := u.torrent.Stats()
		b.WriteString(
			fmt.Sprintf(" seeding:%v peers(total/active):%v/%v read/write:%v/%v",
				u.torrent.Seeding(), stats.TotalPeers, stats.ActivePeers,
				stats.BytesRead, stats.BytesWritten))
		s := u.torrent.PieceState(0)
		b.WriteString(
			fmt.Sprintf(" piece[0]checking:%v complete:%v ok:%v partial:%v priority:%v",
				s.Checking, s.Complete, s.Ok, s.Partial, s.Priority))
	}
	return b.String()
}

func (u *Update) deploy() {
	if u.deployFails > DeployFailsLimit {
		log.Printf("Too many deployment failures:%d uuid:%s version:%d",
			u.deployFails, u.Metainfo.UUID, u.Metainfo.Version)
		return
	}

	var err error

	log.Printf("deploying update uuid:%s version:%d", u.Metainfo.UUID, u.Metainfo.Version)
	switch u.Metainfo.UUID {
	//case UUIDApk:
	//	u.deployWithApk()
	case UUIDShell:
		err = u.deployWithShell()
	default:
		u.deployFails++
		log.Printf("ERROR: Unrecognized uuid:%s", u.Metainfo.UUID)
		return
	}

	if err != nil {
		u.deployFails++
	} else {
		u.deployFails = 0
		u.Deployed = time.Now()
	}
}

/*func (u *Update) deployWithApk() {
}*/

func (u *Update) deployWithShell() error {
	for _, f := range u.torrent.Files() {
		log.Printf("executing update shell uuid:%s version:%d file:%s",
			u.Metainfo.UUID, u.Metainfo.Version, f.Path())
		cmd := exec.Command("timeout", "-t", strconv.Itoa(ShellExecutionTimeout), "/bin/sh", f.Path())
		if err := cmd.Run(); err != nil {
			log.Printf("ERROR: failed executing update shell uuid:%s version:%d file:%s",
				u.Metainfo.UUID, u.Metainfo.Version, f.Path())
			return err
		}
		log.Printf("executed update shell script uuid:%s version:%d file:%s",
			u.Metainfo.UUID, u.Metainfo.Version, f.Path())
	}
	return nil
}

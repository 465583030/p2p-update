package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

	Metainfo Metainfo  `json:"metainfo,omitempty"`
	Deployed time.Time `json:"deployed,omitempty"`
	Source   string    `json:"source,omitempty"`

	torrent     *torrent.Torrent
	stopped     bool
	sent        bool
	deployFails int
	agent       *Agent
}

// NewUpdateFromMessage creates an Update instance from a byte-array of torrent++.
func NewUpdateFromMessage(b []byte, a *Agent) (*Update, error) {
	u := Update{
		stopped: true,
		sent:    false,
		agent:   a,
	}
	if err := bencode.DecodeBytes(b, &u.Metainfo); err != nil {
		return nil, err
	}
	return &u, nil
}

// LoadUpdateFromFile loads Update description from given filename.
func LoadUpdateFromFile(filename string, a *Agent) (*Update, error) {
	u := Update{
		stopped: true,
		sent:    false,
		agent:   a,
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	return &u, json.NewDecoder(f).Decode(&u)
}

// MetadataFilename returns the name of the update metadata file.
func (u *Update) MetadataFilename() string {
	filename := fmt.Sprintf("%s-v%d", u.Metainfo.UUID, u.Metainfo.Version)
	return filepath.Join(u.agent.Config.BitTorrent.MetadataDir, filename)
}

// Save writes Update metadata to file.
func (u *Update) Save() error {
	u.RLock()
	defer u.RUnlock()
	filename := u.MetadataFilename()
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		return err
	}
	return json.NewEncoder(f).Encode(u)
}

// Verify verifies the update. It returns an error if the verification fails,
// otherwise nil.
func (u *Update) Verify(a *Agent) error {
	if err := u.Metainfo.Verify(*a.PublicKey); err != nil {
		log.Printf("verification failed: %v", err)
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
		toSave := true

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
			toSave = false
		}
	}
}

// Stop stops the lifecycle of the update.
func (u *Update) Stop() {
	u.Lock()
	defer u.Unlock()
	log.Printf("stopping update: %v", u.String())
	u.stopped = true
	if u.torrent != nil {
		u.torrent.Drop()
		<-u.torrent.Closed()
	}
	log.Printf("stopped update: %v", u.String())
}

// Delete deletes this update files.
func (u *Update) Delete() error {
	u.Lock()
	defer u.Unlock()
	log.Printf("deleting update: %v", u.String())
	if !u.stopped {
		return fmt.Errorf("update has not been stopped")
	}
	if u.torrent != nil {
		for _, f := range u.torrent.Files() {
			filename := filepath.Join(u.agent.Config.BitTorrent.DataDir, f.Path())
			if _, err := os.Stat(filename); err == nil {
				if err = os.Remove(filename); err != nil {
					return err
				}
			}
		}
	}
	filename := u.MetadataFilename()
	if _, err := os.Stat(filename); err == nil {
		if err := os.Remove(filename); err != nil {
			return err
		}
	}
	log.Printf("deleted update: %v", u.String())
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
	var err error

	for _, f := range u.torrent.Files() {
		script := filepath.Join(u.agent.Config.BitTorrent.DataDir, f.Path())
		cmd := exec.Command("/bin/sh", script)
		log.Printf("executing update shell uuid:%s version:%d file:%s",
			u.Metainfo.UUID, u.Metainfo.Version, script)
		if err = cmd.Start(); err != nil {
			log.Printf("ERROR: failed executing update shell uuid:%s version:%d file:%s - %v",
				u.Metainfo.UUID, u.Metainfo.Version, f.Path(), err)
			break
		}
		timer := time.AfterFunc(ShellExecutionTimeout*time.Second, func() {
			cmd.Process.Kill()
		})
		err = cmd.Wait()
		timer.Stop()
		if err == nil {
			log.Printf("executed update shell script uuid:%s version:%d file:%s",
				u.Metainfo.UUID, u.Metainfo.Version, f.Path())
		} else {
			log.Printf("ERROR: executed update shell with error uuid:%s version:%d file:%s - %v",
				u.Metainfo.UUID, u.Metainfo.Version, f.Path(), err)
			break
		}
	}
	return err
}

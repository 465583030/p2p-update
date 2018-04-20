package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"

	"github.com/anacrolix/torrent/metainfo"
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

	Notification Notification `json:"notification"`
	Deployed     time.Time    `json:"deployed"`
	Source       string       `json:"source"`
	Stopped      bool         `json:"stopped"`
	Sent         bool         `json:"sent"`
	DeployFails  int          `json:"deploy-fails"`
	Missing      int64        `json:"missing"`

	torrent *torrent.Torrent
	agent   *Agent
}

// NewUpdate returns an Update instance from given notification and agent.
func NewUpdate(n Notification, a *Agent) *Update {
	return &Update{
		Notification: n,
		Stopped:      true,
		Sent:         false,
		agent:        a,
	}
}

// LoadUpdateFromFile loads Update description from given filename.
func LoadUpdateFromFile(filename string, a *Agent) (*Update, error) {
	u := Update{
		Stopped: true,
		Sent:    false,
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
	filename := fmt.Sprintf("%s-v%d", u.Notification.UUID, u.Notification.Version)
	return filepath.Join(u.agent.metadataDir, filename)
}

// Save writes Update metadata to file.
func (u *Update) Save() error {
	f, err := os.OpenFile(u.MetadataFilename(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		return err
	}
	return u.Write(f)
}

// Write writes this Update instance to Writer 'w'.
func (u *Update) Write(w io.Writer) error {
	u.RLock()
	defer u.RUnlock()
	return json.NewEncoder(w).Encode(u)
}

// Verify verifies the update. It returns an error if the verification fails,
// otherwise nil.
func (u *Update) Verify(a *Agent) error {
	if err := u.Notification.Verify(a.PublicKey); err != nil {
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
		old *Update
		err error
	)

	if err = u.Verify(a); err != nil {
		return err
	}

	// Remove existing update that has the same UUID. If the existing update
	// is newer, then return an error.
	if old, err = a.addUpdate(u); err != nil {
		return err
	}
	if old == nil {
		log.Printf("older update of uuid:%s does not exist", u.Notification.UUID)
	} else {
		old.Stop()
		if err = old.Delete(); err != nil {
			log.Printf("WARNING: failed to delete update uuid:%s version:%d - %v",
				old.Notification.UUID, old.Notification.Version, err)
		}
	}

	// activate torrent
	log.Printf("starting update: %s", u.String())
	if mi, err = u.Notification.torrentMetainfo(); err != nil {
		return fmt.Errorf("failed generating torrent metainfo: %v", err)
	}
	if u.torrent, err = a.torrentClient.AddTorrent(mi); err != nil {
		return fmt.Errorf("failed adding torrent: %v", err)
	}
	u.Stopped = false
	log.Printf("started update: %s", u.String())

	// spawn a go-routine that monitors torrent's status
	go u.monitor(a)

	return nil
}

func (u *Update) monitor(a *Agent) {
	toSave := true
	for {
		time.Sleep(5 * time.Second)

		u.Lock()
		if u.Stopped || u.torrent == nil {
			break
		}
		if !u.Sent {
			if err := u.Notification.Write(a.Overlay); err != nil {
				log.Printf("failed sending update uuid:%s version:%d : %v",
					u.Notification.UUID, u.Notification.Version, err)
			} else {
				u.Sent = true
				toSave = true
			}
		}
		u.Missing = u.torrent.BytesMissing()
		if u.Missing > 0 {
			<-u.torrent.GotInfo()
			u.torrent.DownloadAll()
		} else if !a.Config.Proxy && u.Deployed.Year() < 2000 {
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
	u.Stopped = true
	if u.torrent != nil {
		u.torrent.Drop()
		<-u.torrent.Closed()
		u.torrent = nil
	}
	log.Printf("stopped update: %v", u.String())
}

// Delete deletes this update files.
func (u *Update) Delete() error {
	u.Lock()
	defer u.Unlock()
	log.Printf("deleting update: %v", u.String())
	if !u.Stopped {
		return fmt.Errorf("update has not been stopped")
	}

	filename := filepath.Join(u.agent.dataDir, u.Notification.Info.Name)
	if err := os.RemoveAll(filename); err != nil {
		log.Printf("WARNING: failed removing update file %s", filename)
	}

	filename = u.MetadataFilename()
	if err := os.RemoveAll(filename); err != nil {
		return errors.Wrapf(err, "failed deleting update uuid:%s version:%d",
			u.Notification.UUID, u.Notification.Version)
	}

	log.Printf("deleted update: %v", u.String())
	return nil
}

func (u *Update) String() string {
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("uuid:%v version:%d", u.Notification.UUID, u.Notification.Version))
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
	if u.DeployFails > DeployFailsLimit {
		log.Printf("Too many deployment failures:%d uuid:%s version:%d",
			u.DeployFails, u.Notification.UUID, u.Notification.Version)
		return
	}

	var (
		apk   ApkDeployer
		shell ShellDeployer
		err   error
	)

	log.Printf("deploying update uuid:%s version:%d", u.Notification.UUID, u.Notification.Version)
	switch u.Notification.UUID {
	case UUIDApk:
		err = u.deployWith(apk)
	case UUIDShell:
		err = u.deployWith(shell)
	default:
		u.DeployFails++
		log.Printf("ERROR: Unrecognized uuid:%s", u.Notification.UUID)
		return
	}

	if err != nil {
		u.DeployFails++
	} else {
		u.DeployFails = 0
		u.Deployed = time.Now()
	}
}

func (u *Update) deployWith(d Deployer) error {
	for _, f := range u.torrent.Files() {
		script := filepath.Join(u.agent.dataDir, f.Path())
		log.Printf("executing update shell uuid:%s version:%d file:%s",
			u.Notification.UUID, u.Notification.Version, script)
		if err := d.deploy(script, ShellExecutionTimeout*time.Second); err != nil {
			log.Printf("ERROR: executed update shell with error uuid:%s version:%d file:%s - %v",
				u.Notification.UUID, u.Notification.Version, f.Path(), err)
			return err
		}
		log.Printf("executed update shell script uuid:%s version:%d file:%s",
			u.Notification.UUID, u.Notification.Version, f.Path())
	}
	return nil
}

// Deployer is an interface of update deployer.
type Deployer interface {
	deploy(filename string, d time.Duration) error
}

// ShellDeployer is an update deployer using system shell.
type ShellDeployer struct{}

func (sh ShellDeployer) deploy(filename string, d time.Duration) error {
	st, err := os.Stat(filename)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return sh.deployDir(filename, d)
	}
	return sh.deployFile(filename, d)
}

func (ShellDeployer) deployFile(filename string, d time.Duration) error {
	cmd := exec.Command("/bin/sh", filename)
	if err := cmd.Start(); err != nil {
		return err
	}
	timer := time.AfterFunc(d, func() {
		cmd.Process.Kill()
	})
	err := cmd.Wait()
	timer.Stop()
	return err
}

func (sh ShellDeployer) deployDir(filename string, d time.Duration) error {
	main := fmt.Sprintf("%s/main.sh", filename)
	if _, err := os.Stat(main); err != nil {
		return err
	}
	return sh.deployFile(main, d)
}

// ApkDeployer is an update deployer using APK (Alpine Package Management).
type ApkDeployer struct{}

func (ApkDeployer) deploy(filename string, d time.Duration) error {
	// TODO: implement
	return fmt.Errorf("not implemented")
}

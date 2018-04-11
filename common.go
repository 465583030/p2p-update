package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spacemonkeygo/openssl"

	"github.com/gortc/stun"
	"github.com/pkg/errors"
	"github.com/vmihailenco/msgpack"
)

const (
	// DefaultTracker is the default BitTorrent tracker address
	DefaultTracker = "https://fruit-testbed.org:443/announce"

	// DefaultPieceLength is the default length of BitTorrent file-piece
	DefaultPieceLength = 32 * 1024
)

const (
	signatureName = "org.fruit-testbed"
	softwareName  = "fruit/p2p-update"

	stunPassword          = "123"
	stunMaxPacketDataSize = 56 * 1024
)

var (
	stunDataRequest           = stun.NewType(stun.MethodData, stun.ClassRequest)
	stunDataSuccess           = stun.NewType(stun.MethodData, stun.ClassSuccessResponse)
	stunDataError             = stun.NewType(stun.MethodData, stun.ClassErrorResponse)
	stunBindingIndication     = stun.NewType(stun.MethodBinding, stun.ClassIndication)
	stunChannelBindIndication = stun.NewType(stun.MethodChannelBind, stun.ClassIndication)

	errNonSTUNMessage = errors.New("Not STUN Message")
)

// PeerMessage is a message sent by a peer.
type PeerMessage []byte

// AddTo writes a PeerMessage on given STUN message.
func (pd PeerMessage) AddTo(m *stun.Message) error {
	m.Add(stun.AttrData, pd)
	return nil
}

// PeerID is an identifier of peer.
type PeerID [6]byte

func (pid PeerID) String() string {
	return hex.EncodeToString(pid[:])
}

// AddTo writes a PeerID on given STUN message.
func (pid *PeerID) AddTo(m *stun.Message) error {
	m.Add(stun.AttrUsername, pid[:])
	return nil
}

// GetFrom gets a PeerID from given STUN message.
func (pid *PeerID) GetFrom(m *stun.Message) error {
	var (
		buf []byte
		err error
	)

	if buf, err = m.Get(stun.AttrUsername); err != nil {
		return errors.Wrap(err, "cannot get username from the message")
	} else if len(buf) != len(pid) {
		return fmt.Errorf("length of username (%d bytes) is not 6 bytes", len(buf))
	}
	for i, b := range buf {
		pid[i] = b
	}
	return nil
}

// TorrentPorts holds (external, internal) ports of torrent client.
type TorrentPorts [2]int

// AddTo adds TorrentPorts into STUN message.
func (tp *TorrentPorts) AddTo(m *stun.Message) error {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[:4], uint32(tp[0]))
	binary.LittleEndian.PutUint32(b[4:], uint32(tp[1]))
	m.Add(stun.AttrEvenPort, b)
	return nil
}

// GetFrom gets TorrentPorts from STUN message.
func (tp *TorrentPorts) GetFrom(m *stun.Message) error {
	b, err := m.Get(stun.AttrEvenPort)
	if err == nil {
		tp[0] = int(binary.LittleEndian.Uint32(b[:4]))
		tp[1] = int(binary.LittleEndian.Uint32(b[4:]))
	}
	return err
}

// SessionTable is a map whose keys are Peer IDs
// and values are pairs of [external-addr, internal-addr].
type SessionTable map[PeerID][]*net.UDPAddr

// JSON marshals the SessionTable to JSON and then returns it.
func (st *SessionTable) JSON() []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	i := 0
	for pid, addrs := range *st {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('"')
		buf.WriteString(pid.String())
		buf.WriteByte('"')
		buf.WriteByte(':')
		buf.WriteByte('[')
		for j, addr := range addrs {
			if j > 0 {
				buf.WriteByte(',')
			}
			buf.WriteByte('"')
			buf.WriteString(addr.String())
			buf.WriteByte('"')
		}
		buf.WriteByte(']')
		i++
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// AddTo marshals a SessionTable as MessagePack data, then
// writes it on given STUN message as AttrData.
func (st *SessionTable) AddTo(m *stun.Message) error {
	var (
		data []byte
		err  error
	)

	if data, err = msgpack.Marshal(st); err == nil {
		m.Add(stun.AttrData, data)
	}
	return err
}

// GetSessionTableFrom reads a MessagePack data from AttrData of given
// STUN message, then unmarshals and returns it as SessionTable.
func GetSessionTableFrom(m *stun.Message) (*SessionTable, error) {
	var (
		st   SessionTable
		data []byte
		err  error
	)

	if data, err = m.Get(stun.AttrData); err == nil {
		err = msgpack.Unmarshal(data, &st)
	}
	return &st, err
}

func validateMessage(m *stun.Message, t *stun.MessageType) error {
	var (
		err error
	)

	if t != nil && (m.Type.Method != t.Method || m.Type.Class != t.Class) {
		return fmt.Errorf("incorrect message type, expected %v but got %v",
			*t, m.Type)
	}

	var username stun.Username
	if err = username.GetFrom(m); err != nil {
		return fmt.Errorf("invalid username: %v", err)
	}

	if err = stun.Fingerprint.Check(m); err != nil {
		return fmt.Errorf("fingerprint is incorrect: %v", err)
	}

	i := stun.NewShortTermIntegrity(stunPassword)
	if err = i.Check(m); err != nil {
		return fmt.Errorf("Integrity bad: %v", err)
	}

	return nil
}

// RaspberryPiSerial returns the board serial number retrieved from /proc/cpuinfo
func RaspberryPiSerial() (*PeerID, error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return nil, errors.Wrap(err, "cannot open /proc/cpuinfo")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > 10 && line[0:7] == "Serial\t" {
			var (
				pid    PeerID
				serial []byte
			)

			s := strings.TrimLeft(strings.Split(line, " ")[1], "0")
			if serial, err = hex.DecodeString(s); err != nil {
				return nil, errors.Wrapf(err, "failed converting %s to []byte", s)
			}
			j := len(pid) - 1
			for i := len(serial) - 1; i >= 0 && j >= 0; i-- {
				pid[j] = serial[i]
				j--
			}
			return &pid, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.Wrap(err, "failed to read serial number")
	}
	return nil, errors.New("cannot find serial number from /proc/cpuinfo")
}

// ActiveMacAddress returns a MAC address of active network interface.
// Note that ActiveMacAddress iterates the interfaces returned by `net.Interfaces`
// from first to the last, and returns the first active interface.
func ActiveMacAddress() ([]byte, error) {
	var (
		ifaces []net.Interface
		err    error
	)
	if ifaces, err = net.Interfaces(); err != nil {
		return nil, err
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp != 0 && bytes.Compare(i.HardwareAddr, nil) != 0 {
			// Don't use random as we have a real address
			return i.HardwareAddr, nil
		}
	}
	return nil, errors.New("No active ethernet available")
}

// LocalPeerID returns a PeerID of local machine.
// If the machine is Raspberry Pi, then it returns the board serial number.
// Otherwise, it returns the MAC address of the first active network interface.
func LocalPeerID() (*PeerID, error) {
	if serial, err := RaspberryPiSerial(); err == nil {
		return serial, nil
	}

	var pid PeerID
	if mac, err := ActiveMacAddress(); err == nil && len(mac) >= 6 {
		for i, b := range mac {
			pid[i] = b
		}
		return &pid, nil
	}
	return nil, errors.New("CPU serial and active ethernet are not available")
}

// ExecEvery periodically executes function `f` every `t`. It returns a channel
// that can be closed in order to stop this periodic execution.
func ExecEvery(t time.Duration, f func()) chan struct{} {
	ticker := time.NewTicker(t)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				f()
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
	return quit
}

// LoadPrivateKey reads and returns a private-key from given filename.
func LoadPrivateKey(filename string) (openssl.PrivateKey, error) {
	var (
		key openssl.PrivateKey
		b   []byte
		err error
	)

	if b, err = ioutil.ReadFile(filename); err == nil {
		key, err = openssl.LoadPrivateKeyFromPEM(b)
	}
	return key, err
}

// LoadPublicKey reads and returns a public-key from given filename.
func LoadPublicKey(filename string) (openssl.PublicKey, error) {
	var (
		key openssl.PublicKey
		b   []byte
		err error
	)

	if b, err = ioutil.ReadFile(filename); err == nil {
		key, err = openssl.LoadPublicKeyFromPEM(b)
	}
	return key, err
}

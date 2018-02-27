package main

import (
  "fmt"
  "net"
  "bytes"
  "os"
  "bufio"
  "strings"

  "github.com/gortc/stun"
  "github.com/pkg/errors"
)

var (
  stunRealm = "fruit-testbed.org"
  stunSoftware = stun.NewSoftware("fruit/p2psecureupdate")
  stunPassword = "123"
  errNonSTUNMessage = errors.New("Not STUN Message")
)

func ValidMessage(m *stun.Message) (bool, error) {
  var soft stun.Software
  var err error

  if err = soft.GetFrom(m); err != nil {
    return false, err
  } else if soft.String() != stunSoftware.String() {
    return false, nil
  }

  var username stun.Username
  if err = username.GetFrom(m); err != nil {
    return false, err
  }

  if err = stun.Fingerprint.Check(m); err != nil {
    return false, errors.New(fmt.Sprintf("fingerprint is incorrect: %v", err))
  }

  i := stun.NewShortTermIntegrity(stunPassword)
  if err = i.Check(m); err != nil {
    return false, errors.New(fmt.Sprintf("Integrity bad: %v", err))
  }

  return true, nil
}

func piSerial() (string, error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", errors.New("Cannot open /proc/cpuinfo")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > 10 && line[0:7] == "Serial\t" {
			return strings.TrimLeft(strings.Split(line, " ")[1], "0"), nil
		}
	}
	if err := scanner.Err(); err != nil {
		errors.Wrap(err, "Failed to read serial number")
	}
	return "", errors.New("Cannot find serial number from /proc/cpuinfo")
}

func getActiveMacAddress() (string, error) {
	if interfaces, err := net.Interfaces(); err != nil {
    return "", err
  } else {
		for _, i := range interfaces {
			if i.Flags&net.FlagUp != 0 && bytes.Compare(i.HardwareAddr, nil) != 0 {
				// Don't use random as we have a real address
				return i.HardwareAddr.String(), nil
			}
		}
	}
	return "", errors.New("No active ethernet available")
}

func localUsername() (string, error) {
	if serial, err := piSerial(); err == nil {
		return serial, nil
	}
  if mac, err := getActiveMacAddress(); err == nil {
		return strings.Replace(mac, ":", "", -1), nil
	}
  if hostname, err := os.Hostname(); err == nil {
    return hostname, nil
  }
  return "", errors.New("CPU serial, active ethernet, and hostname are not available")
}
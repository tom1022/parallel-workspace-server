package session

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

const (
	procNetTCP  = "/proc/net/tcp"
	procNetTCP6 = "/proc/net/tcp6"

	// tcpEstablished is the state column's value for a live connection. The
	// listening socket and half-closed ones carry other values and are not
	// sessions.
	tcpEstablished = "01"
)

// SSHSessions reports how many SSH sessions currently hold this workspace
// (4.8). It counts established connections to the endpoint's port rather than
// sshd's own processes: the process tree's shape is an OpenSSH implementation
// detail that changed with the sshd-session split, whereas the socket table is
// the same on every version.
func SSHSessions(port int) (int, error) {
	total := 0
	for _, path := range []string{procNetTCP, procNetTCP6} {
		f, err := os.Open(path)
		if err != nil {
			// tcp6 is absent on a host with IPv6 disabled.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return 0, err
		}
		n, err := countEstablished(f, port)
		f.Close()
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func countEstablished(r io.Reader, port int) (int, error) {
	count := 0
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[3] != tcpEstablished {
			continue
		}
		_, hexPort, found := strings.Cut(fields[1], ":")
		if !found {
			continue
		}
		local, err := strconv.ParseUint(hexPort, 16, 32)
		if err != nil {
			continue
		}
		if int(local) == port {
			count++
		}
	}
	return count, scanner.Err()
}

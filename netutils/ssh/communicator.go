package ssh

import (
	"bufio"
	"bytes"
	"code.google.com/p/go.crypto/ssh"
	"errors"
	"fmt"
	"github.com/mitchellh/packer/packer"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
)

type comm struct {
	client  *ssh.Client
	config  *Config
	conn    net.Conn
	address string
}

// Config is the structure used to configure the SSH communicator.
type Config struct {
	// The configuration of the Go SSH connection
	SSHConfig *ssh.ClientConfig

	// Connection returns a new connection. The current connection
	// in use will be closed as part of the Close method, or in the
	// case an error occurs.
	Connection func() (net.Conn, error)

	// NoPty, if true, will not request a pty from the remote end.
	NoPty bool
}

// Creates a new packer.Communicator implementation over SSH. This takes
// an already existing TCP connection and SSH configuration.
func New(address string, config *Config) (result *comm, err error) {
	// Establish an initial connection and connect
	result = &comm{
		config:  config,
		address: address,
	}

	if err = result.reconnect(); err != nil {
		result = nil
		return
	}

	return
}

func (c *comm) Start(cmd *packer.RemoteCmd) (err error) {
	session, err := c.newSession()
	if err != nil {
		return
	}

	// Setup our session
	session.Stdin = cmd.Stdin
	session.Stdout = cmd.Stdout
	session.Stderr = cmd.Stderr

	if !c.config.NoPty {
		// Request a PTY
		termModes := ssh.TerminalModes{
			ssh.ECHO:          0,     // do not echo
			ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
			ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
		}

		if err = session.RequestPty("xterm", 80, 40, termModes); err != nil {
			return
		}
	}

	log.Printf("starting remote command: %s", cmd.Command)
	err = session.Start(cmd.Command + "\n")
	if err != nil {
		return
	}

	// A channel to keep track of our done state
	doneCh := make(chan struct{})
	sessionLock := new(sync.Mutex)
	timedOut := false

	// Start a goroutine to wait for the session to end and set the
	// exit boolean and status.
	go func() {
		defer session.Close()

		err := session.Wait()
		exitStatus := 0
		if err != nil {
			exitErr, ok := err.(*ssh.ExitError)
			if ok {
				exitStatus = exitErr.ExitStatus()
			}
		}

		sessionLock.Lock()
		defer sessionLock.Unlock()

		if timedOut {
			// We timed out, so set the exit status to -1
			exitStatus = -1
		}

		log.Printf("remote command exited with '%d': %s", exitStatus, cmd.Command)
		cmd.SetExited(exitStatus)
		close(doneCh)
	}()

	return
}

func (c *comm) Upload(path string, input io.Reader, fi *os.FileInfo) error {
	// The target directory and file for talking the SCP protocol
	target_dir := filepath.Dir(path)
	target_file := filepath.Base(path)

	// On windows, filepath.Dir uses backslash seperators (ie. "\tmp").
	// This does not work when the target host is unix.  Switch to forward slash
	// which works for unix and windows
	target_dir = filepath.ToSlash(target_dir)

	scpFunc := func(w io.Writer, stdoutR *bufio.Reader) error {
		return scpUploadFile(target_file, input, w, stdoutR, fi)
	}

	return c.scpSession("scp -qrt "+target_dir, scpFunc)
}

func (c *comm) UploadDir(dst string, src string, excl []string) error {
	log.Printf("Upload dir '%s' to '%s'", src, dst)
	scpFunc := func(w io.Writer, r *bufio.Reader) error {
		uploadEntries := func() error {
			f, err := os.Open(src)
			if err != nil {
				return err
			}
			defer f.Close()

			entries, err := f.Readdir(-1)
			if err != nil {
				return err
			}

			return scpUploadDir(src, entries, w, r)
		}

		if src[len(src)-1] != '/' {
			log.Printf("No trailing slash, creating the source directory name")
			fi, err := os.Stat(src)
			if err != nil {
				return err
			}
			return scpUploadDirProtocol(filepath.Base(src), w, r, uploadEntries, fi)
		} else {
			// Trailing slash, so only upload the contents
			return uploadEntries()
		}
	}

	return c.scpSession("scp -rvt "+dst, scpFunc)
}

func (c *comm) Download(string, io.Writer) error {
	panic("not implemented yet")
}

func (c *comm) newSession() (session *ssh.Session, err error) {
	log.Println("opening new ssh session")
	if c.client == nil {
		err = errors.New("client not available")
	} else {
		session, err = c.client.NewSession()
	}

	if err != nil {
		log.Printf("ssh session open error: '%s', attempting reconnect", err)
		if err := c.reconnect(); err != nil {
			return nil, err
		}

		return c.client.NewSession()
	}

	return session, nil
}

func (c *comm) reconnect() (err error) {
	if c.conn != nil {
		c.conn.Close()
	}

	// Set the conn and client to nil since we'll recreate it
	c.conn = nil
	c.client = nil

	log.Printf("reconnecting to TCP connection for SSH")
	c.conn, err = c.config.Connection()
	if err != nil {
		// Explicitly set this to the REAL nil. Connection() can return
		// a nil implementation of net.Conn which will make the
		// "if c.conn == nil" check fail above. Read here for more information
		// on this psychotic language feature:
		//
		// http://golang.org/doc/faq#nil_error
		c.conn = nil

		log.Printf("reconnection error: %s", err)
		return
	}

	log.Printf("handshaking with SSH")
	sshConn, sshChan, req, err := ssh.NewClientConn(c.conn, c.address, c.config.SSHConfig)
	if err != nil {
		log.Printf("handshake error: %s", err)
	}
	if sshConn != nil {
		c.client = ssh.NewClient(sshConn, sshChan, req)
	}

	return
}

func (c *comm) scpSession(scpCommand string, f func(io.Writer, *bufio.Reader) error) error {
	session, err := c.newSession()
	if err != nil {
		return err
	}
	defer session.Close()

	// Get a pipe to stdin so that we can send data down
	stdinW, err := session.StdinPipe()
	if err != nil {
		return err
	}

	// We only want to close once, so we nil w after we close it,
	// and only close in the defer if it hasn't been closed already.
	defer func() {
		if stdinW != nil {
			stdinW.Close()
		}
	}()

	// Get a pipe to stdout so that we can get responses back
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stdoutR := bufio.NewReader(stdoutPipe)

	// Set stderr to a bytes buffer
	stderr := new(bytes.Buffer)
	session.Stderr = stderr

	// Start the sink mode on the other side
	// TODO(mitchellh): There are probably issues with shell escaping the path
	log.Println("Starting remote scp process: ", scpCommand)
	if err := session.Start(scpCommand); err != nil {
		return err
	}

	// Call our callback that executes in the context of SCP. We ignore
	// EOF errors if they occur because it usually means that SCP prematurely
	// ended on the other side.
	log.Println("Started SCP session, beginning transfers...")
	if err := f(stdinW, stdoutR); err != nil && err != io.EOF {
		return err
	}

	// Close the stdin, which sends an EOF, and then set w to nil so that
	// our defer func doesn't close it again since that is unsafe with
	// the Go SSH package.
	log.Println("SCP session complete, closing stdin pipe.")
	stdinW.Close()
	stdinW = nil

	// Wait for the SCP connection to close, meaning it has consumed all
	// our data and has completed. Or has errored.
	log.Println("Waiting for SSH session to complete.")
	err = session.Wait()
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			// Otherwise, we have an ExitErorr, meaning we can just read
			// the exit status
			log.Printf("non-zero exit status: %d", exitErr.ExitStatus())

			// If we exited with status 127, it means SCP isn't available.
			// Return a more descriptive error for that.
			if exitErr.ExitStatus() == 127 {
				return errors.New(
					"SCP failed to start. This usually means that SCP is not\n" +
						"properly installed on the remote system.")
			}
		}

		return err
	}

	log.Printf("scp stderr (length %d): %s", stderr.Len(), stderr.String())
	return nil
}

// checkSCPStatus checks that a prior command sent to SCP completed
// successfully. If it did not complete successfully, an error will
// be returned.
func checkSCPStatus(r *bufio.Reader) error {
	code, err := r.ReadByte()
	if err != nil {
		return err
	}

	if code != 0 {
		// Treat any non-zero (really 1 and 2) as fatal errors
		message, _, err := r.ReadLine()
		if err != nil {
			return fmt.Errorf("Error reading error message: %s", err)
		}

		return errors.New(string(message))
	}

	return nil
}

func scpUploadFile(dst string, src io.Reader, w io.Writer, r *bufio.Reader, fi *os.FileInfo) error {
	var mode os.FileMode
	var size int64

	if fi != nil && (*fi).Mode().IsRegular() {
		mode = (*fi).Mode().Perm()
		size = (*fi).Size()
	} else {
		// Create a temporary file where we can copy the contents of the src
		// so that we can determine the length, since SCP is length-prefixed.
		tf, err := ioutil.TempFile("", "packer-upload")
		if err != nil {
			return fmt.Errorf("Error creating temporary file for upload: %s", err)
		}
		defer os.Remove(tf.Name())
		defer tf.Close()

		mode = 0644

		log.Println("Copying input data into temporary file so we can read the length")
		if _, err := io.Copy(tf, src); err != nil {
			return err
		}

		// Sync the file so that the contents are definitely on disk, then
		// read the length of it.
		if err := tf.Sync(); err != nil {
			return fmt.Errorf("Error creating temporary file for upload: %s", err)
		}

		// Seek the file to the beginning so we can re-read all of it
		if _, err := tf.Seek(0, 0); err != nil {
			return fmt.Errorf("Error creating temporary file for upload: %s", err)
		}

		tfi, err := tf.Stat()
		if err != nil {
			return fmt.Errorf("Error creating temporary file for upload: %s", err)
		}

		size = tfi.Size()
		src = tf
	}

	// Start the protocol
	perms := fmt.Sprintf("C%04o", mode)
	log.Printf("[DEBUG] Uploading %s: perms=%s size=%d", dst, perms, size)

	fmt.Fprintln(w, perms, size, dst)
	if err := checkSCPStatus(r); err != nil {
		return err
	}

	if _, err := io.CopyN(w, src, size); err != nil {
		return err
	}

	fmt.Fprint(w, "\x00")
	if err := checkSCPStatus(r); err != nil {
		return err
	}

	return nil
}

func scpUploadDirProtocol(name string, w io.Writer, r *bufio.Reader, f func() error, fi os.FileInfo) error {
	log.Printf("SCP: starting directory upload: %s", name)

	mode := fi.Mode().Perm()

	perms := fmt.Sprintf("D%04o 0", mode)

	fmt.Fprintln(w, perms, name)
	err := checkSCPStatus(r)
	if err != nil {
		return err
	}

	if err := f(); err != nil {
		return err
	}

	fmt.Fprintln(w, "E")
	if err != nil {
		return err
	}

	return nil
}

func scpUploadDir(root string, fs []os.FileInfo, w io.Writer, r *bufio.Reader) error {
	for _, fi := range fs {
		realPath := filepath.Join(root, fi.Name())

		// Track if this is actually a symlink to a directory. If it is
		// a symlink to a file we don't do any special behavior because uploading
		// a file just works. If it is a directory, we need to know so we
		// treat it as such.
		isSymlinkToDir := false
		if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
			symPath, err := filepath.EvalSymlinks(realPath)
			if err != nil {
				return err
			}

			symFi, err := os.Lstat(symPath)
			if err != nil {
				return err
			}

			isSymlinkToDir = symFi.IsDir()
		}

		if !fi.IsDir() && !isSymlinkToDir {
			// It is a regular file (or symlink to a file), just upload it
			f, err := os.Open(realPath)
			if err != nil {
				return err
			}

			err = func() error {
				defer f.Close()
				return scpUploadFile(fi.Name(), f, w, r, &fi)
			}()

			if err != nil {
				return err
			}

			continue
		}

		// It is a directory, recursively upload
		err := scpUploadDirProtocol(fi.Name(), w, r, func() error {
			f, err := os.Open(realPath)
			if err != nil {
				return err
			}
			defer f.Close()

			entries, err := f.Readdir(-1)
			if err != nil {
				return err
			}

			return scpUploadDir(realPath, entries, w, r)
		}, fi)
		if err != nil {
			return err
		}
	}

	return nil
}

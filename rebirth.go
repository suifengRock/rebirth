// ps aux|grep zj | grep -v grep | awk '{print $2}' | xargs -n1 -t kill -SIGUSR1

package rebirth

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	REBIRTH_TAG     = "BornOfFire"
	REBIRTH_ENV_KEY = "GO_REBIRTH_TAG"
	PRE_SIGNAL      = iota
	POST_SIGNAL

	STATE_INIT
	STATE_RUNNING
	STATE_SHUTTING_DOWN
	STATE_TERMINATE
)

type RebirthServer struct {
	http.Server

	ln         net.Listener
	hasRebirth bool
	hasRunFork bool
	sigChan    chan os.Signal
	wg         *sync.WaitGroup
	state      uint8
	mutex      *sync.Mutex
}

func NewServer(addr string, handler http.Handler) (svr *RebirthServer) {
	hasRebirth := os.Getenv("GO_REBIRTH_TAG") != ""

	svr = &RebirthServer{
		hasRebirth: hasRebirth,
		wg:         new(sync.WaitGroup),
		sigChan:    make(chan os.Signal),
		state:      STATE_INIT,
		mutex:      new(sync.Mutex),
	}
	svr.Server.Addr = addr
	svr.Server.MaxHeaderBytes = http.DefaultMaxHeaderBytes
	svr.Server.Handler = handler
	return
}

func ListenAndServe(addr string, handler http.Handler) error {
	svr := NewServer(addr, handler)
	return svr.ListenAndServe()
}

func (svr *RebirthServer) ListenAndServe() error {
	addr := svr.Addr
	if addr == "" {
		addr = ":http"
	}

	ln, err := svr.getListener(addr)
	if err != nil {
		return err
	}

	svr.ln = newListener(ln, svr)

	return svr.Serve()
}

func (svr *RebirthServer) Serve() (err error) {
	go svr.acceptProcessSign()
	if svr.hasRebirth {
		syscall.Kill(syscall.Getppid(), syscall.SIGUSR2)
	}

	svr.setState(STATE_RUNNING)
	err = svr.Server.Serve(svr.ln)

	svr.wg.Wait()
	svr.setState(STATE_TERMINATE)
	return
}

func (svr *RebirthServer) getListener(addr string) (l net.Listener, err error) {
	if svr.hasRebirth {
		//
		f := os.NewFile(3, "")
		l, err = net.FileListener(f)
		if err != nil {
			return
		}
	} else {
		l, err = net.Listen("tcp", addr)
		if err != nil {
			return
		}
	}

	return
}

func (svr *RebirthServer) setState(state uint8) {
	svr.state = state
}

func (svr *RebirthServer) acceptProcessSign() {
	var sig os.Signal

	signal.Notify(
		svr.sigChan,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	)

	pid := syscall.Getpid()
	for {
		sig = <-svr.sigChan
		switch sig {
		case syscall.SIGUSR1:
			log.Println(pid, "Received SIGHUP. forking.")
			err := svr.fork()
			if err != nil {
				log.Println("Fork err:", err)
			}
		case syscall.SIGUSR2:
			log.Println(pid, "Received SIGINT.")
			svr.shutdown()
		default:
			log.Printf("Received %v: nothing i care about...\n", sig)
		}
	}
}

func (svr *RebirthServer) fork() (err error) {
	svr.mutex.Lock()

	if svr.hasRunFork {
		errors.New("The process already forked...")
	}
	svr.hasRunFork = true
	svr.mutex.Unlock()

	files := make([]*os.File, 1)
	files[0] = svr.ln.(*RebirthListener).File()

	env := append(os.Environ(), fmt.Sprintf("%s=%s", REBIRTH_ENV_KEY, REBIRTH_TAG))

	path := os.Args[0]
	var args []string
	if len(os.Args) > 1 {
		args = os.Args[1:]
	}

	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = files
	cmd.Env = env
	err = cmd.Start()
	if err != nil {
		log.Fatalf("Restart: Failed to launch, error: %v", err)
	}

	return
}

func (svr *RebirthServer) shutdown() {
	if svr.state != STATE_RUNNING {
		return
	}
	svr.SetKeepAlivesEnabled(false)
	err := svr.ln.Close()
	if err != nil {
		log.Println(syscall.Getpid(), "Listener.Close() error:", err)
	} else {
		log.Println(syscall.Getpid(), svr.ln.Addr(), "Listener closed.")
	}
}

type RebirthListener struct {
	net.Listener
	server  *RebirthServer
	stopped bool
}

func newListener(l net.Listener, svr *RebirthServer) (ln *RebirthListener) {
	ln = &RebirthListener{
		Listener: l,
		server:   svr,
	}
	return
}

func (ln *RebirthListener) Accept() (c net.Conn, err error) {
	tc, err := ln.Listener.(*net.TCPListener).AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)

	ln.server.wg.Add(1)
	c = NewConn(tc, ln.server)
	return c, nil
}

func (ln *RebirthListener) File() *os.File {
	tl := ln.Listener.(*net.TCPListener)
	fl, _ := tl.File()
	return fl
}

func (ln *RebirthListener) Close() error {
	if ln.stopped {
		return syscall.EINVAL
	}
	ln.stopped = true
	return ln.Listener.Close()
}

type Conn struct {
	net.Conn
	server *RebirthServer
}

func NewConn(c net.Conn, s *RebirthServer) *Conn {
	return &Conn{
		Conn:   c,
		server: s,
	}
}

func (c *Conn) Close() error {
	c.server.wg.Done()
	return c.Conn.Close()
}

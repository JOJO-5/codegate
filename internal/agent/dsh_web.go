package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/processutil"
	"github.com/jojo/codegate/internal/protocol"
)

const dshLoopback = "127.0.0.1:3080"

var webAuthority = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,251}[a-z0-9](?::[0-9]{1,5})?$`)

type webStream struct {
	mu    sync.Mutex
	conn  net.Conn
	queue chan []byte
	done  chan struct{}
	once  sync.Once
}

func (s *webStream) close() {
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.mu.Unlock()
	})
}

func (a *Agent) onWebStart(req *protocol.Envelope) {
	go func() {
		finish, activityErr := a.beginActivity()
		if activityErr != nil {
			a.replyError(req, activityErr)
			return
		}
		defer finish()
		p, err := protocol.DecodePayload[protocol.WebStartPayload](req)
		if err != nil || !a.dshWebEnabled() || !validWebAuthority(p.Host) ||
			strings.Contains(p.Host, "..") || !strings.Contains(p.Host, ".") {
			a.replyError(req, errors.New("DSH Web is not enabled or host is invalid"))
			return
		}
		a.webMu.Lock()
		defer a.webMu.Unlock()
		if a.webProcess != nil && a.webHost == p.Host && a.webCookie != "" {
			a.reply(req, protocol.TypeWebStarted, protocol.WebStartedPayload{Cookie: a.webCookie})
			return
		}
		if err := a.stopWebProcessLocked(); err != nil {
			a.replyError(req, errors.New("previous DSH Web process tree could not be stopped"))
			return
		}
		a.closeWebStreams()
		binary, err := exec.LookPath("dsh")
		if err != nil {
			a.replyError(req, errors.New("dsh is not installed for the Agent service account"))
			return
		}
		cmd := dshWebCommand(binary, p.Host)
		cmd.Env = BuildEnv(a.cfg, 80, 24)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			a.replyError(req, err)
			return
		}
		cmd.Stderr = io.Discard // DSH may print credentials; never copy them into Agent logs.
		tree, err := processutil.StartTree(cmd)
		if err != nil {
			a.replyError(req, err)
			return
		}
		exited := make(chan struct{})
		a.webProcess, a.webTree, a.webDone = cmd, tree, exited
		go func() {
			_ = cmd.Wait()
			cleanupErr := tree.Stop()
			close(exited)
			a.webMu.Lock()
			if a.webProcess == cmd {
				a.webCookie = ""
				if cleanupErr == nil {
					a.webProcess, a.webTree, a.webDone = nil, nil, nil
				}
			}
			a.webMu.Unlock()
		}()
		ready := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 4096), 1<<20)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if strings.HasPrefix(line, "dsh web: http://") {
					select {
					case ready <- strings.TrimPrefix(line, "dsh web: "):
					default:
					}
				}
			}
		}()
		var launchURL string
		timer := time.NewTimer(40 * time.Second)
		defer timer.Stop()
		select {
		case launchURL = <-ready:
		case <-exited:
		case <-timer.C:
		}
		cookie, err := bootstrapDSH(launchURL, p.Host)
		if err != nil {
			if stopErr := a.stopWebProcessLocked(); stopErr != nil {
				a.replyError(req, errors.New("DSH Web startup failed and process tree cleanup failed"))
				return
			}
			a.replyError(req, errors.New("DSH Web did not start or browser authentication failed"))
			return
		}
		select {
		case <-exited:
			a.replyError(req, errors.New("DSH Web exited during browser authentication"))
			return
		default:
		}
		a.webProcess, a.webTree, a.webDone = cmd, tree, exited
		a.webHost, a.webCookie = p.Host, cookie
		a.reply(req, protocol.TypeWebStarted, protocol.WebStartedPayload{Cookie: cookie})
	}()
}

// bootstrapDSH exchanges DSH's process token locally. The returned signed
// cookie stays on the Server-Agent channel and is never sent to the browser.
func bootstrapDSH(rawURL, host string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || u.Host != dshLoopback || u.Scheme != "http" || u.Query().Get("token") == "" {
		return "", errors.New("DSH startup URL is missing a loopback token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Host = host
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		return "", errors.New("DSH did not exchange browser token")
	}
	for _, c := range res.Cookies() {
		if strings.HasPrefix(c.Name, "dsh-auth-") && c.Value != "" {
			return c.Name + "=" + c.Value, nil
		}
	}
	return "", errors.New("DSH browser cookie missing")
}

// Caller holds webMu. Waiters close webDone before acquiring that mutex.
func (a *Agent) stopWebProcessLocked() error {
	if a.webTree != nil {
		if err := a.webTree.Stop(); err != nil {
			return err
		}
	}
	if a.webDone != nil {
		<-a.webDone
	}
	a.webProcess, a.webTree, a.webDone = nil, nil, nil
	a.webCookie, a.webHost = "", ""
	return nil
}

func (a *Agent) stopWeb() error {
	a.webMu.Lock()
	defer a.webMu.Unlock()
	err := a.stopWebProcessLocked()
	a.closeWebStreams()
	return err
}

func (a *Agent) webActive() bool {
	a.webMu.Lock()
	defer a.webMu.Unlock()
	return a.webProcess != nil
}

func (a *Agent) closeWebStreams() {
	a.webStreamsMu.Lock()
	streams := a.webStreams
	a.webStreams = nil
	a.webStreamsMu.Unlock()
	for _, s := range streams {
		s.close()
	}
}

func (a *Agent) removeWebStream(id uuid.UUID, s *webStream) {
	a.webStreamsMu.Lock()
	if a.webStreams[id] == s {
		delete(a.webStreams, id)
	}
	a.webStreamsMu.Unlock()
	s.close()
}

func (a *Agent) onWebOpen(req *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.WebStreamPayload](req)
	id, parseErr := uuid.Parse(p.StreamID)
	if err != nil || parseErr != nil || !a.dshWebEnabled() {
		return
	}
	a.webMu.Lock()
	ready := a.webProcess != nil && a.webCookie != ""
	a.webMu.Unlock()
	if !ready {
		return
	}
	s := &webStream{queue: make(chan []byte, 16), done: make(chan struct{})}
	a.webStreamsMu.Lock()
	if len(a.webStreams) >= 32 {
		a.webStreamsMu.Unlock()
		return
	}
	if a.webStreams == nil {
		a.webStreams = make(map[uuid.UUID]*webStream)
	}
	a.webStreams[id] = s
	a.webStreamsMu.Unlock()
	go func() {
		defer a.removeWebStream(id, s)
		local, err := net.DialTimeout("tcp", dshLoopback, 5*time.Second)
		if err != nil {
			a.sendWebClose(id)
			return
		}
		s.mu.Lock()
		select {
		case <-s.done:
			s.mu.Unlock()
			_ = local.Close()
			return
		default:
		}
		s.conn = local
		s.mu.Unlock()
		go func() {
			for {
				select {
				case <-s.done:
					return
				case data := <-s.queue:
					if len(data) == 0 {
						continue
					}
					if _, err := local.Write(data); err != nil {
						s.close()
						return
					}
				}
			}
		}()
		buf := make([]byte, 16<<10)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				frame, encErr := protocol.EncodeFrame(protocol.FrameWebToServer, 0, id, buf[:n])
				if encErr != nil {
					break
				}
				conn := a.currentConn()
				if conn == nil || conn.SendBinaryReliable(frame) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		a.sendWebClose(id)
	}()
}

func (a *Agent) sendWebClose(id uuid.UUID) {
	env, err := protocol.NewEnvelope(protocol.TypeWebClose, protocol.WebStreamPayload{StreamID: id.String()})
	if err == nil {
		_ = a.sendControl(env)
	}
}

func (a *Agent) onWebClose(env *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.WebStreamPayload](env)
	if err != nil {
		return
	}
	id, err := uuid.Parse(p.StreamID)
	if err != nil {
		return
	}
	a.webStreamsMu.Lock()
	s := a.webStreams[id]
	delete(a.webStreams, id)
	a.webStreamsMu.Unlock()
	if s != nil {
		s.close()
	}
}

func (a *Agent) onWebFrame(f protocol.Frame) {
	a.webStreamsMu.Lock()
	s := a.webStreams[f.StreamID]
	a.webStreamsMu.Unlock()
	if s == nil {
		return
	}
	if len(f.Payload) > 16<<10 {
		a.removeWebStream(f.StreamID, s)
		a.sendWebClose(f.StreamID)
		return
	}
	data := append([]byte(nil), f.Payload...)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-s.done:
	case s.queue <- data:
	case <-timer.C:
		a.removeWebStream(f.StreamID, s)
		a.sendWebClose(f.StreamID)
	}
}

func validWebAuthority(host string) bool {
	if !webAuthority.MatchString(host) {
		return false
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		port, err := strconv.Atoi(host[i+1:])
		return err == nil && port > 0 && port <= 65535
	}
	return true
}

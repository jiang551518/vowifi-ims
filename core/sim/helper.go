// Package sim talks to the privileged RootHelper (Java, app_process, uid 0)
// which exposes the SIM/UICC functions Android reserves for privileged
// callers: AKA authentication, subscriber identities and logical channel APDU
// access. The helper protocol is newline-delimited JSON over stdin/stdout.
package sim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const helperClass = "app.vowifi.root.RootHelper"

// Helper is a running RootHelper process. It is safe for concurrent use.
type Helper struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan json.RawMessage
	nextID  atomic.Int64
	done    chan struct{}
	err     error
	tailMu  sync.Mutex
	errTail []string // last stderr lines, reported when the helper fails to start
	Log     func(string)
}

// StartHelper launches RootHelper with the given CLASSPATH (an APK or a dex
// jar). When the current process is not root it escalates through su.
func StartHelper(ctx context.Context, classpath, suPath string) (*Helper, error) {
	if strings.ContainsAny(classpath, "'\"\n") {
		return nil, errors.New("sim: invalid helper classpath")
	}
	// The helper runs as the system uid (1000) rather than root: telephony
	// services verify that the calling package belongs to the calling uid,
	// and "android" belongs to uid 1000 which also holds every privileged
	// telephony permission.
	if suPath == "" || suPath == "su" {
		suPath = findSU()
	}
	script := fmt.Sprintf("[ -n \"$BOOTCLASSPATH\" ] || echo 'BOOTCLASSPATH missing in helper environment' >&2; "+
		"CLASSPATH='%s' exec /system/bin/app_process /system/bin %s android", classpath, helperClass)
	cmd := exec.Command(suPath, "1000", "-c", script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sim: start root helper: %w", err)
	}
	h := &Helper{cmd: cmd, stdin: stdin, pending: map[int64]chan json.RawMessage{}, done: make(chan struct{})}
	ready := make(chan error, 1)
	go h.readLoop(stdout, ready)
	if stderr != nil {
		go func() {
			sc := bufio.NewScanner(stderr)
			for sc.Scan() {
				line := sc.Text()
				h.tailMu.Lock()
				h.errTail = append(h.errTail, line)
				if len(h.errTail) > 8 {
					h.errTail = h.errTail[len(h.errTail)-8:]
				}
				h.tailMu.Unlock()
				if h.Log != nil {
					h.Log("roothelper: " + line)
				}
			}
		}()
	}
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	select {
	case err := <-ready:
		if err != nil {
			time.Sleep(200 * time.Millisecond) // let stderr drain
			h.tailMu.Lock()
			tail := strings.Join(h.errTail, " | ")
			h.tailMu.Unlock()
			h.Close()
			if tail != "" {
				return nil, fmt.Errorf("%w: %s", err, tail)
			}
			return nil, err
		}
	case <-timeout.C:
		h.Close()
		return nil, errors.New("sim: root helper did not start (root permission denied?)")
	case <-ctx.Done():
		h.Close()
		return nil, ctx.Err()
	}
	return h, nil
}

func (h *Helper) readLoop(r io.Reader, ready chan<- error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		var head struct {
			ID    int64 `json:"id"`
			Ready bool  `json:"ready"`
		}
		if json.Unmarshal(line, &head) != nil {
			if first { // su / app_process messages before the protocol starts
				h.tailMu.Lock()
				h.errTail = append(h.errTail, "stdout: "+string(line))
				if len(h.errTail) > 8 {
					h.errTail = h.errTail[len(h.errTail)-8:]
				}
				h.tailMu.Unlock()
			}
			continue
		}
		if first && head.Ready {
			first = false
			ready <- nil
			continue
		}
		h.mu.Lock()
		ch := h.pending[head.ID]
		delete(h.pending, head.ID)
		h.mu.Unlock()
		if ch != nil {
			ch <- append(json.RawMessage(nil), line...)
		}
	}
	err := sc.Err()
	if err == nil {
		err = errors.New("sim: root helper exited")
		if h.cmd != nil && h.cmd.Process != nil {
			if werr := h.cmd.Wait(); werr != nil {
				err = fmt.Errorf("sim: root helper exited (%v)", werr)
			} else {
				err = errors.New("sim: root helper exited (status 0)")
			}
		}
	}
	if first {
		ready <- err
	}
	h.mu.Lock()
	h.err = err
	for id, ch := range h.pending {
		close(ch)
		delete(h.pending, id)
	}
	h.mu.Unlock()
	close(h.done)
}

// Close terminates the helper process.
func (h *Helper) Close() error {
	if h == nil {
		return nil
	}
	_ = h.stdin.Close()
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
	return nil
}

// Done is closed when the helper exits.
func (h *Helper) Done() <-chan struct{} { return h.done }

type envelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Call sends one request. req must marshal to a JSON object; op is injected.
func (h *Helper) Call(ctx context.Context, op string, req map[string]any, resp any) error {
	id := h.nextID.Add(1)
	msg := map[string]any{"id": id, "op": op}
	for k, v := range req {
		msg[k] = v
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ch := make(chan json.RawMessage, 1)
	h.mu.Lock()
	if h.err != nil {
		h.mu.Unlock()
		return h.err
	}
	h.pending[id] = ch
	h.mu.Unlock()
	h.writeMu.Lock()
	_, err = h.stdin.Write(append(body, '\n'))
	h.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("sim: write to root helper: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case raw, ok := <-ch:
		if !ok {
			return errors.New("sim: root helper exited")
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return err
		}
		if !env.OK {
			return fmt.Errorf("sim: %s: %s", op, env.Error)
		}
		if resp != nil {
			return json.Unmarshal(raw, resp)
		}
		return nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return ctx.Err()
	}
}

// Subscription is one active SIM subscription as reported by Android.
type Subscription struct {
	SubID       int    `json:"subId"`
	Slot        int    `json:"slot"`
	MCC         string `json:"mcc"`
	MNC         string `json:"mnc"`
	CarrierName string `json:"carrierName"`
	DisplayName string `json:"displayName"`
	ICCID       string `json:"iccid"`
	CountryISO  string `json:"countryIso"`
	Embedded    bool   `json:"embedded"`
	CardID      int    `json:"cardId"`
	PortIndex   int    `json:"portIndex"`
	CarrierID   int    `json:"carrierId"`
}

func (h *Helper) Subscriptions(ctx context.Context) ([]Subscription, error) {
	var resp struct {
		Subs []Subscription `json:"subs"`
	}
	err := h.Call(ctx, "subs", nil, &resp)
	return resp.Subs, err
}

// Identity holds the subscriber identities needed for ePDG and IMS.
type Identity struct {
	IMSI   string   `json:"imsi"`
	ICCID  string   `json:"iccid"`
	MSISDN string   `json:"msisdn"`
	GID1   string   `json:"gid1"`
	IMPI   string   `json:"impi"`
	Domain string   `json:"domain"`
	IMPU   []string `json:"impu"`
	SMSC   string   `json:"smsc"`
	IMEI   string   `json:"imei"`
	SVN    string   `json:"svn"`
}

func (h *Helper) Identity(ctx context.Context, subID, slot int) (Identity, error) {
	var id Identity
	err := h.Call(ctx, "identity", map[string]any{"subId": subID, "slot": slot}, &id)
	return id, err
}

// Card describes one UICC slot.
type Card struct {
	CardID       int    `json:"cardId"`
	EID          string `json:"eid"`
	ICCID        string `json:"iccid"`
	PhysicalSlot int    `json:"physicalSlot"`
	EUICC        bool   `json:"euicc"`
	Removable    bool   `json:"removable"`
}

func (h *Helper) Cards(ctx context.Context) ([]Card, error) {
	var resp struct {
		Cards []Card `json:"cards"`
	}
	err := h.Call(ctx, "cards", nil, &resp)
	return resp.Cards, err
}

// findSU locates the su binary by absolute path: inside Android app processes
// PATH may be empty, so exec.LookPath("su") is not reliable.
func findSU() string {
	for _, p := range []string{"/system/bin/su", "/system/xbin/su", "/data/adb/ksu/bin/su", "/debug_ramdisk/su", "/sbin/su", "/su/bin/su"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "su"
}

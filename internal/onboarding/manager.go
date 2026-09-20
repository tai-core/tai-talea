// Package onboarding owns durable SSH installation jobs. The browser only
// submits intent; this worker runs inside Talea and resumes on process restart.
package onboarding

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/store"
)

type Request struct {
	RequestID     string `json:"request_id"`
	ID            string `json:"id"`
	PartnerID     string `json:"partner_id"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password,omitempty"`
	PrivateKey    string `json:"private_key,omitempty"`
	HostKey       string `json:"host_key,omitempty"`
	AdvertiseHost string `json:"advertise_host,omitempty"`
}

type Job struct {
	JobID         string    `json:"job_id"`
	LeaseID       string    `json:"lease_id,omitempty"`
	ID            string    `json:"id"`
	PartnerID     string    `json:"partner_id"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	User          string    `json:"user"`
	AdvertiseHost string    `json:"advertise_host,omitempty"`
	State         string    `json:"state"`
	Stage         string    `json:"stage"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type record struct {
	Job    Job              `json:"job"`
	Secret []byte           `json:"secret,omitempty"`
	Result *domain.Instance `json:"result,omitempty"`
}

type Runner func(context.Context, Request, func(string) error) (domain.Instance, error)
type Enroll func(context.Context, domain.Instance) error
type Manager struct {
	mu     sync.Mutex
	runMu  sync.Mutex
	dir    string
	key    cipher.AEAD
	jobs   map[string]record
	store  store.Store
	run    Runner
	enroll Enroll
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var ErrConflict = errors.New("节点已纳管、正在上线，或请求编号已被使用")

func New(dir string, persistence store.Store, run Runner, enroll Enroll) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("onboarding.state_dir is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "credentials.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		err = atomicWrite(keyPath, key)
	}
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, key: aead, jobs: map[string]record{}, store: persistence, run: run, enroll: enroll}
	files, err := filepath.Glob(filepath.Join(dir, "job-*.json"))
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var rec record
		if err = json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("read onboarding job: %w", err)
		}
		if !safeID.MatchString(rec.Job.JobID) {
			return nil, errors.New("invalid stored job id")
		}
		m.jobs[rec.Job.JobID] = rec
	}
	return m, nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	// Linux deployment: sync the directory entry as well as the file content.
	if d, e := os.Open(filepath.Dir(path)); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (m *Manager) save(rec record) error {
	rec.Job.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(m.dir, "job-"+rec.Job.JobID+".json"), data); err != nil {
		return err
	}
	m.jobs[rec.Job.JobID] = rec
	return nil
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	return len(host) <= 253 && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`).MatchString(host)
}

func (m *Manager) Submit(ctx context.Context, r Request) (Job, error) {
	if !safeID.MatchString(r.ID) || !safeID.MatchString(r.RequestID) || !safeID.MatchString(r.User) || !validHost(r.Host) || r.Port < 1 || r.Port > 65535 ||
		(r.AdvertiseHost != "" && !validHost(r.AdvertiseHost)) || (r.Password == "") == (r.PrivateKey == "") || len(r.Password) > 4096 || len(r.PrivateKey) > 16384 {
		return Job{}, errors.New("请填写有效节点名、SSH 地址、端口、账号，以及密码或私钥中的一项")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.jobs[r.RequestID]; ok {
		if old.Job.PartnerID != r.PartnerID || old.Job.ID != r.ID || old.Job.Host != r.Host || old.Job.Port != r.Port || old.Job.User != r.User || old.Job.AdvertiseHost != r.AdvertiseHost {
			return Job{}, ErrConflict
		}
		return old.Job, nil
	}
	active := 0
	for _, rec := range m.jobs {
		if rec.Job.State == "QUEUED" || rec.Job.State == "RUNNING" {
			active++
			if rec.Job.ID == r.ID || (rec.Job.Host == r.Host && rec.Job.Port == r.Port) {
				return Job{}, ErrConflict
			}
		}
	}
	if active >= 100 {
		return Job{}, errors.New("上线队列已满，请稍后重试")
	}
	row, err := m.store.GetInstance(ctx, r.ID)
	if err == nil && (row.PartnerID != r.PartnerID || row.InstanceState != domain.InstanceReleased) {
		return Job{}, ErrConflict
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Job{}, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return Job{}, err
	}
	nonce := make([]byte, m.key.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return Job{}, err
	}
	secret := m.key.Seal(nonce, nonce, data, []byte(r.RequestID))
	job := Job{JobID: r.RequestID, ID: r.ID, PartnerID: r.PartnerID, Host: r.Host, Port: r.Port, User: r.User, AdvertiseHost: r.AdvertiseHost, State: "QUEUED", Stage: "等待接管", CreatedAt: time.Now().UTC()}
	if err = m.save(record{Job: job, Secret: secret}); err != nil {
		return Job{}, err
	}
	return m.jobs[job.JobID].Job, nil
}

func (m *Manager) List(partner string) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := []Job{}
	for _, rec := range m.jobs {
		if partner == "" || rec.Job.PartnerID == partner {
			job := rec.Job
			// Use the persisted installation result, including for jobs created
			// before lease_id was exposed. A reused node name is a different lease.
			if rec.Result != nil {
				job.LeaseID = rec.Result.LeaseID
			}
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	return jobs
}

// RunOnce is sequential: two jobs cannot install over the same endpoint.
func (m *Manager) RunOnce(ctx context.Context) error {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	var rec record
	for _, candidate := range m.jobs {
		if (candidate.Job.State == "QUEUED" || candidate.Job.State == "RUNNING") && (rec.Job.JobID == "" || candidate.Job.CreatedAt.Before(rec.Job.CreatedAt)) {
			rec = candidate
		}
	}
	if rec.Job.JobID == "" {
		m.mu.Unlock()
		return nil
	}
	rec.Job.State = "RUNNING"
	if err := m.save(rec); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	progress := func(stage string) error { m.mu.Lock(); defer m.mu.Unlock(); rec.Job.Stage = stage; return m.save(rec) }
	var err error
	if rec.Result == nil {
		var r Request
		if len(rec.Secret) < m.key.NonceSize() {
			err = errors.New("上线凭据缺失，请重新提交")
		} else {
			n := m.key.NonceSize()
			var raw []byte
			raw, err = m.key.Open(nil, rec.Secret[:n], rec.Secret[n:], []byte(rec.Job.JobID))
			if err == nil {
				err = json.Unmarshal(raw, &r)
			}
		}
		if err == nil {
			// Refuse changed ownership or an intervening online operation before SSH.
			current, e := m.store.GetInstance(ctx, r.ID)
			if e == nil && (current.PartnerID != r.PartnerID || current.InstanceState != domain.InstanceReleased) {
				err = ErrConflict
			}
			if e != nil && !errors.Is(e, store.ErrNotFound) {
				err = e
			}
		}
		if err == nil {
			var instance domain.Instance
			instance, err = m.run(ctx, r, progress)
			if err == nil {
				instance.ID, instance.PartnerID, instance.LeaseID = r.ID, r.PartnerID, "ssh-"+r.RequestID
				rec.Result = &instance
				rec.Secret = nil
				err = progress("环境就绪，提交纳管")
			}
		}
	}
	if err == nil {
		err = m.enroll(ctx, *rec.Result)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	} // leave a resumable record on shutdown
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// A per-job deadline is a terminal failure. Retrying a timed-out RUNNING
		// job forever would block every newer job and retain its credentials.
		err = errors.New("上线任务超时，请检查网络和节点安装日志后重新提交")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		rec.Job.State = "FAILED"
		rec.Job.Error = err.Error()
		rec.Job.Stage = "上线失败"
	} else {
		rec.Job.State = "MANAGED"
		rec.Job.Stage = "已接管，自动分配 P/D"
	}
	// No credentials survive success or a terminal failure. Retry requires entry.
	rec.Secret = nil
	return m.save(rec)
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jobCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
			_ = m.RunOnce(jobCtx)
			cancel()
		}
	}
}

// ProcessRunner executes only the configured, operator-owned program. SSH
// values travel in JSON on stdin; none can become a shell argument or log line.
func ProcessRunner(cfg config.OnboardingConfig, bootstrapToken string) Runner {
	return func(ctx context.Context, r Request, progress func(string) error) (domain.Instance, error) {
		input, _ := json.Marshal(map[string]any{"request": r, "bootstrap_token": bootstrapToken, "manifest": cfg.Manifest, "state_dir": cfg.StateDir})
		cmd := exec.CommandContext(ctx, cfg.Python, cfg.Script)
		cmd.WaitDelay = 5 * time.Second
		cmd.Stdin = bytes.NewReader(input)
		cmd.Stderr = io.Discard
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return domain.Instance{}, err
		}
		if err = cmd.Start(); err != nil {
			return domain.Instance{}, errors.New("无法启动安装程序，请检查控制节点的上线配置")
		}
		var result domain.Instance
		var reported string
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			var line struct {
				Stage  string           `json:"stage"`
				Error  string           `json:"error"`
				Result *domain.Instance `json:"result"`
			}
			if json.Unmarshal(scanner.Bytes(), &line) != nil {
				continue
			}
			if line.Stage != "" {
				if e := progress(line.Stage); e != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return result, e
				}
			}
			if line.Error != "" {
				reported = line.Error
			}
			if line.Result != nil {
				result = *line.Result
			}
		}
		if scanner.Err() != nil {
			// Stop before Wait: an oversized output line otherwise leaves a child
			// blocked writing to a pipe that nobody is reading anymore.
			_ = cmd.Process.Kill()
		}
		err = cmd.Wait()
		if err != nil || scanner.Err() != nil || result.Endpoint == "" {
			if reported == "" {
				reported = "安装程序未完成；请检查 SSH 连通性和控制节点安装日志"
			}
			for _, secret := range []string{r.Password, r.PrivateKey, bootstrapToken} {
				if secret != "" {
					reported = strings.ReplaceAll(reported, secret, "[redacted]")
				}
			}
			return result, errors.New(reported)
		}
		return result, nil
	}
}

func NewRequestID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

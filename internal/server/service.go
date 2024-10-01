package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

const (
	B  int64 = 1
	KB       = B << 10
	MB       = KB << 10
	GB       = MB << 10
)

const (
	DefaultDeployTimeout = time.Second * 30
	DefaultDrainTimeout  = time.Second * 30
	DefaultPauseTimeout  = time.Second * 30

	DefaultHealthCheckPath     = "/up"
	DefaultHealthCheckInterval = time.Second
	DefaultHealthCheckTimeout  = time.Second * 5

	MaxIdleConnsPerHost = 100
	ProxyBufferSize     = 32 * KB

	DefaultTargetTimeout       = time.Second * 30
	DefaultMaxMemoryBufferSize = 1 * MB
	DefaultMaxRequestBodySize  = 0
	DefaultMaxResponseBodySize = 0

	DefaultStopMessage = ""
)

var ErrorRolloutTargetNotSet = errors.New("rollout target not set")

type TargetSlot int

const (
	TargetSlotActive TargetSlot = iota
	TargetSlotRollout
)

type HealthCheckConfig struct {
	Path     string        `json:"path"`
	Interval time.Duration `json:"interval"`
	Timeout  time.Duration `json:"timeout"`
}

type ServiceOptions struct {
	TLSEnabled    bool   `json:"tls_enabled"`
	ACMEDirectory string `json:"acme_directory"`
	ACMECachePath string `json:"acme_cache_path"`
	ErrorPagePath string `json:"error_page_path"`
}

func (so ServiceOptions) ScopedCachePath() string {
	// We need to scope our certificate cache according to whatever ACME settings
	// we want to use, such as the directory.  This is so we can reuse
	// certificates between deployments when the settings are the same, but
	// provision new certificates when they change.

	hasher := sha256.New()
	hasher.Write([]byte(so.ACMEDirectory))
	hash := hex.EncodeToString(hasher.Sum(nil))

	return path.Join(so.ACMECachePath, hash)
}

type Service struct {
	name    string
	hosts   []string
	options ServiceOptions

	active     *Target
	rollout    *Target
	targetLock sync.RWMutex

	pauseController   *PauseController
	rolloutController *RolloutController
	certManager       *autocert.Manager
	middleware        http.Handler
}

func NewService(name string, hosts []string, options ServiceOptions) *Service {
	service := &Service{
		name:    name,
		hosts:   hosts,
		options: options,
	}

	service.initialize()

	return service
}

func (s *Service) UpdateOptions(hosts []string, options ServiceOptions) {
	s.hosts = hosts
	s.options = options
	s.certManager = s.createCertManager()
	s.middleware = s.createMiddleware()
}

func (s *Service) ActiveTarget() *Target {
	s.targetLock.RLock()
	defer s.targetLock.RUnlock()

	return s.active
}

func (s *Service) RolloutTarget() *Target {
	s.targetLock.RLock()
	defer s.targetLock.RUnlock()

	return s.rollout
}

func (s *Service) ClaimTarget(req *http.Request) (*Target, *http.Request, error) {
	s.targetLock.RLock()
	defer s.targetLock.RUnlock()

	target := s.active
	if s.rollout != nil && s.rolloutController != nil && s.rolloutController.RequestUsesRolloutGroup(req) {
		slog.Debug("Using rollout target for request", "service", s.name, "path", req.URL.Path)
		target = s.rollout
	}

	req, err := target.StartRequest(req)
	return target, req, err
}

func (s *Service) SetTarget(slot TargetSlot, target *Target, drainTimeout time.Duration) {
	s.targetLock.Lock()
	defer s.targetLock.Unlock()

	var replaced *Target

	switch slot {
	case TargetSlotActive:
		replaced = s.active
		s.active = target

	case TargetSlotRollout:
		replaced = s.rollout
		s.rollout = target
	}

	if replaced != nil {
		replaced.StopHealthChecks()
		replaced.Drain(drainTimeout)
	}
}

func (s *Service) SetRolloutSplit(percentage int, allowlist []string) error {
	s.targetLock.Lock()
	defer s.targetLock.Unlock()

	if s.rollout == nil {
		return ErrorRolloutTargetNotSet
	}

	s.rolloutController = NewRolloutController(percentage, allowlist)
	slog.Info("Set rollout split", "service", s.name, "percentage", percentage, "allowlist", allowlist)
	return nil
}

func (s *Service) StopRollout() error {
	s.targetLock.Lock()
	defer s.targetLock.Unlock()

	s.rolloutController = nil
	slog.Info("Stopped rollout", "service", s.name)
	return nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.middleware.ServeHTTP(w, r)
}

type marshalledService struct {
	Name              string             `json:"name"`
	Hosts             []string           `json:"hosts"`
	ActiveTarget      string             `json:"active_target"`
	RolloutTarget     string             `json:"rollout_target"`
	Options           ServiceOptions     `json:"options"`
	TargetOptions     TargetOptions      `json:"target_options"`
	PauseController   *PauseController   `json:"pause_controller"`
	RolloutController *RolloutController `json:"rollout_controller"`
}

func (s *Service) MarshalJSON() ([]byte, error) {
	activeTarget := s.active.Target()
	rolloutTarget := ""
	if s.rollout != nil {
		rolloutTarget = s.rollout.Target()
	}
	targetOptions := s.active.options

	return json.Marshal(marshalledService{
		Name:              s.name,
		Hosts:             s.hosts,
		ActiveTarget:      activeTarget,
		RolloutTarget:     rolloutTarget,
		Options:           s.options,
		TargetOptions:     targetOptions,
		PauseController:   s.pauseController,
		RolloutController: s.rolloutController,
	})
}

func (s *Service) UnmarshalJSON(data []byte) error {
	var ms marshalledService
	err := json.Unmarshal(data, &ms)
	if err != nil {
		return err
	}

	s.name = ms.Name
	s.hosts = ms.Hosts
	s.options = ms.Options
	s.initialize()

	s.pauseController = ms.PauseController
	s.rolloutController = ms.RolloutController
	err = s.restoreSavedTarget(TargetSlotActive, ms.ActiveTarget, ms.TargetOptions)
	if err != nil {
		return err
	}

	err = s.restoreSavedTarget(TargetSlotRollout, ms.RolloutTarget, ms.TargetOptions)
	if err != nil {
		return err
	}

	return nil
}

func (s *Service) Stop(drainTimeout time.Duration, message string) error {
	err := s.pauseController.Stop(message)
	if err != nil {
		return err
	}

	slog.Info("Service stopped", "service", s.name)

	s.ActiveTarget().Drain(drainTimeout)
	slog.Info("Service drained", "service", s.name)
	return nil
}

func (s *Service) Pause(drainTimeout time.Duration, pauseTimeout time.Duration) error {
	err := s.pauseController.Pause(pauseTimeout)
	if err != nil {
		return err
	}

	slog.Info("Service paused", "service", s.name)

	s.ActiveTarget().Drain(drainTimeout)
	slog.Info("Service drained", "service", s.name)
	return nil
}

func (s *Service) Resume() error {
	err := s.pauseController.Resume()
	if err != nil {
		return err
	}

	slog.Info("Service resumed", "service", s.name)
	return nil
}

// Private

func (s *Service) initialize() {
	s.pauseController = NewPauseController()
	s.certManager = s.createCertManager()
	s.middleware = s.createMiddleware()
}

func (s *Service) createCertManager() *autocert.Manager {
	if !s.options.TLSEnabled {
		return nil
	}

	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(s.options.ScopedCachePath()),
		HostPolicy: autocert.HostWhitelist(s.hosts...),
		Client:     &acme.Client{DirectoryURL: s.options.ACMEDirectory},
	}
}

func (s *Service) createMiddleware() http.Handler {
	var handler http.Handler = http.HandlerFunc(s.serviceRequestWithTarget)

	if s.options.ErrorPagePath != "" {
		slog.Debug("Using custom error pages", "service", s.name, "path", s.options.ErrorPagePath)
		errorPageFS := os.DirFS(s.options.ErrorPagePath)
		handler = WithErrorPageMiddleware(errorPageFS, false, handler)
	}

	if s.certManager != nil {
		slog.Debug("Using ACME handler", "service", s.name)
		handler = s.certManager.HTTPHandler(handler)
	}

	return handler
}

func (s *Service) serviceRequestWithTarget(w http.ResponseWriter, r *http.Request) {
	LoggingRequestContext(r).Service = s.name

	if s.options.TLSEnabled && r.TLS == nil {
		s.redirectToHTTPS(w, r)
		return
	}

	if !s.options.TLSEnabled && r.TLS != nil {
		SetErrorResponse(w, r, http.StatusServiceUnavailable, nil)
		return
	}

	if s.handlePausedAndStoppedRequests(w, r) {
		return
	}

	target, req, err := s.ClaimTarget(r)
	if err != nil {
		SetErrorResponse(w, req, http.StatusServiceUnavailable, nil)
		return
	}

	target.SendRequest(w, req)
}

func (s *Service) handlePausedAndStoppedRequests(w http.ResponseWriter, r *http.Request) bool {
	if s.pauseController.GetState() != PauseStateRunning && s.ActiveTarget().IsHealthCheckRequest(r) {
		// When paused or stopped, return success for any health check
		// requests from downstream services. Otherwise, they might consider
		// us as unhealthy while in that state, and remove us from their
		// pool.
		w.WriteHeader(http.StatusOK)
		return true
	}

	action, message := s.pauseController.Wait()
	switch action {
	case PauseWaitActionStopped:
		templateArguments := struct{ Message string }{message}
		SetErrorResponse(w, r, http.StatusServiceUnavailable, templateArguments)
		return true

	case PauseWaitActionTimedOut:
		slog.Warn("Rejecting request due to expired pause", "service", s.name, "path", r.URL.Path)
		SetErrorResponse(w, r, http.StatusGatewayTimeout, nil)
		return true
	}

	return false
}

func (s *Service) restoreSavedTarget(slot TargetSlot, savedTarget string, options TargetOptions) error {
	if savedTarget == "" {
		return nil // Nothing to restore
	}

	target, err := NewTarget(savedTarget, options)
	if err != nil {
		return err
	}

	// Restored targets are always considered healthy, because they would have
	// been that way when they were saved.
	target.state = TargetStateHealthy

	switch slot {
	case TargetSlotActive:
		s.active = target

	case TargetSlotRollout:
		s.rollout = target
	}

	return nil
}

func (s *Service) redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Connection", "close")

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	url := "https://" + host + r.URL.RequestURI()
	http.Redirect(w, r, url, http.StatusMovedPermanently)
}

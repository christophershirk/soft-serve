package server

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/charmbracelet/soft-serve/server/backend"
	cm "github.com/charmbracelet/soft-serve/server/cmd"
	"github.com/charmbracelet/soft-serve/server/config"
	"github.com/charmbracelet/soft-serve/server/hooks"
	"github.com/charmbracelet/soft-serve/server/utils"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	bm "github.com/charmbracelet/wish/bubbletea"
	lm "github.com/charmbracelet/wish/logging"
	rm "github.com/charmbracelet/wish/recover"
	"github.com/muesli/termenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	gossh "golang.org/x/crypto/ssh"
)

var (
	publicKeyCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "public_key_auth_total",
		Help:      "The total number of public key auth requests",
	}, []string{"key", "user", "allowed"})

	keyboardInteractiveCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "keyboard_interactive_auth_total",
		Help:      "The total number of keyboard interactive auth requests",
	}, []string{"user", "allowed"})

	uploadPackCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "git_upload_pack_total",
		Help:      "The total number of git-upload-pack requests",
	}, []string{"key", "user", "repo"})

	receivePackCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "git_receive_pack_total",
		Help:      "The total number of git-receive-pack requests",
	}, []string{"key", "user", "repo"})

	uploadArchiveCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "git_upload_archive_total",
		Help:      "The total number of git-upload-archive requests",
	}, []string{"key", "user", "repo"})

	createRepoCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "soft_serve",
		Subsystem: "ssh",
		Name:      "create_repo_total",
		Help:      "The total number of create repo requests",
	}, []string{"key", "user", "repo"})
)

// SSHServer is a SSH server that implements the git protocol.
type SSHServer struct {
	srv *ssh.Server
	cfg *config.Config
}

// NewSSHServer returns a new SSHServer.
func NewSSHServer(cfg *config.Config, hooks hooks.Hooks) (*SSHServer, error) {
	var err error
	s := &SSHServer{cfg: cfg}
	logger := logger.StandardLog(log.StandardLogOptions{ForceLevel: log.DebugLevel})
	mw := []wish.Middleware{
		rm.MiddlewareWithLogger(
			logger,
			// BubbleTea middleware.
			bm.MiddlewareWithProgramHandler(SessionHandler(cfg), termenv.ANSI256),
			// CLI middleware.
			cm.Middleware(cfg, hooks),
			// Git middleware.
			s.Middleware(cfg),
			// Logging middleware.
			lm.MiddlewareWithLogger(logger),
		),
	}
	s.srv, err = wish.NewServer(
		ssh.PublicKeyAuth(s.PublicKeyHandler),
		ssh.KeyboardInteractiveAuth(s.KeyboardInteractiveHandler),
		wish.WithAddress(cfg.SSH.ListenAddr),
		wish.WithHostKeyPath(filepath.Join(cfg.DataPath, cfg.SSH.KeyPath)),
		wish.WithMiddleware(mw...),
	)
	if err != nil {
		return nil, err
	}

	if cfg.SSH.MaxTimeout > 0 {
		s.srv.MaxTimeout = time.Duration(cfg.SSH.MaxTimeout) * time.Second
	}
	if cfg.SSH.IdleTimeout > 0 {
		s.srv.IdleTimeout = time.Duration(cfg.SSH.IdleTimeout) * time.Second
	}

	return s, nil
}

// ListenAndServe starts the SSH server.
func (s *SSHServer) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Serve starts the SSH server on the given net.Listener.
func (s *SSHServer) Serve(l net.Listener) error {
	return s.srv.Serve(l)
}

// Close closes the SSH server.
func (s *SSHServer) Close() error {
	return s.srv.Close()
}

// Shutdown gracefully shuts down the SSH server.
func (s *SSHServer) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// PublicKeyAuthHandler handles public key authentication.
func (s *SSHServer) PublicKeyHandler(ctx ssh.Context, pk ssh.PublicKey) (allowed bool) {
	if pk == nil {
		return s.cfg.Backend.AllowKeyless()
	}

	ak := backend.MarshalAuthorizedKey(pk)
	defer func() {
		publicKeyCounter.WithLabelValues(ak, ctx.User(), strconv.FormatBool(allowed)).Inc()
	}()

	for _, k := range s.cfg.InitialAdminKeys {
		if k == ak {
			allowed = true
			return
		}
	}

	ac := s.cfg.Backend.AccessLevelByPublicKey("", pk)
	logger.Debugf("access level for %s: %d", ak, ac)
	allowed = ac >= backend.ReadOnlyAccess
	return
}

// KeyboardInteractiveHandler handles keyboard interactive authentication.
func (s *SSHServer) KeyboardInteractiveHandler(ctx ssh.Context, _ gossh.KeyboardInteractiveChallenge) bool {
	ac := s.cfg.Backend.AllowKeyless()
	keyboardInteractiveCounter.WithLabelValues(ctx.User(), strconv.FormatBool(ac)).Inc()
	return ac
}

// Middleware adds Git server functionality to the ssh.Server. Repos are stored
// in the specified repo directory. The provided Hooks implementation will be
// checked for access on a per repo basis for a ssh.Session public key.
// Hooks.Push and Hooks.Fetch will be called on successful completion of
// their commands.
func (s *SSHServer) Middleware(cfg *config.Config) wish.Middleware {
	return func(sh ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			func() {
				cmd := s.Command()
				if len(cmd) >= 2 && strings.HasPrefix(cmd[0], "git") {
					gc := cmd[0]
					// repo should be in the form of "repo.git"
					name := utils.SanitizeRepo(cmd[1])
					pk := s.PublicKey()
					ak := backend.MarshalAuthorizedKey(pk)
					access := cfg.Backend.AccessLevelByPublicKey(name, pk)
					// git bare repositories should end in ".git"
					// https://git-scm.com/docs/gitrepository-layout
					repo := name + ".git"
					reposDir := filepath.Join(cfg.DataPath, "repos")
					if err := ensureWithin(reposDir, repo); err != nil {
						sshFatal(s, err)
						return
					}

					logger.Debug("git middleware", "cmd", gc, "access", access.String())
					repoDir := filepath.Join(reposDir, repo)
					switch gc {
					case receivePackBin:
						if access < backend.ReadWriteAccess {
							sshFatal(s, ErrNotAuthed)
							return
						}
						if _, err := cfg.Backend.Repository(name); err != nil {
							if _, err := cfg.Backend.CreateRepository(name, backend.RepositoryOptions{Private: false}); err != nil {
								log.Errorf("failed to create repo: %s", err)
								sshFatal(s, err)
								return
							}
							createRepoCounter.WithLabelValues(ak, s.User(), name).Inc()
						}
						if err := receivePack(s, s, s.Stderr(), repoDir); err != nil {
							sshFatal(s, ErrSystemMalfunction)
						}
						receivePackCounter.WithLabelValues(ak, s.User(), name).Inc()
						return
					case uploadPackBin, uploadArchiveBin:
						if access < backend.ReadOnlyAccess {
							sshFatal(s, ErrNotAuthed)
							return
						}

						gitPack := uploadPack
						counter := uploadPackCounter
						if gc == uploadArchiveBin {
							gitPack = uploadArchive
							counter = uploadArchiveCounter
						}

						err := gitPack(s, s, s.Stderr(), repoDir)
						if errors.Is(err, ErrInvalidRepo) {
							sshFatal(s, ErrInvalidRepo)
						} else if err != nil {
							sshFatal(s, ErrSystemMalfunction)
						}

						counter.WithLabelValues(ak, s.User(), name).Inc()
					}
				}
			}()
			sh(s)
		}
	}
}

// sshFatal prints to the session's STDOUT as a git response and exit 1.
func sshFatal(s ssh.Session, v ...interface{}) {
	writePktline(s, v...)
	s.Exit(1) // nolint: errcheck
}

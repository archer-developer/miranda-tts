package wyoming

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
)

// Synthesizer produces speech as a stream of 24 kHz/16-bit/mono PCM pieces.
type Synthesizer interface {
	SynthesizeStream(ctx context.Context, text, voice string, emit func(pcm []byte) error) error
}

// Service describes what the server advertises to Home Assistant on describe.
type Service struct {
	Model     string
	Voice     string
	Languages []string
}

// Server listens for Wyoming TCP connections and runs a Session per client.
type Server struct {
	listener net.Listener
	logger   *slog.Logger
	synth    Synthesizer
	service  Service
	seq      atomic.Uint64
}

// NewServer binds addr and returns a Server ready to Serve.
func NewServer(addr string, logger *slog.Logger, synth Synthesizer, service Service) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("wyoming/server: listen %s: %w", addr, err)
	}
	return &Server{listener: ln, logger: logger, synth: synth, service: service}, nil
}

// Addr returns the address the listener is bound to.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Serve accepts connections until ctx is cancelled, then waits for the open
// sessions to finish.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("wyoming/server: accept: %w", err)
			}
		}

		id := fmt.Sprintf("%06d", s.seq.Add(1))
		sess := NewSession(id, conn, s.logger, s.synth, s.service)
		s.logger.Info("client connected", "session_id", id, "remote", conn.RemoteAddr())
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.Run(ctx)
		}()
	}
}

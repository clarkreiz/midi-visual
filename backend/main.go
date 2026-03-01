package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"gitlab.com/gomidi/midi/v2"
	_ "gitlab.com/gomidi/midi/v2/drivers/rtmididrv"
	"gitlab.com/gomidi/midi/v2/smf"
)

// ── broadcaster ──────────────────────────────────────────────────────────────

type broadcaster struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	once    sync.Once
	ready   chan struct{} // closed when first client connects
}

func newBroadcaster() *broadcaster {
	return &broadcaster{clients: make(map[chan []byte]struct{}), ready: make(chan struct{})}
}

func (b *broadcaster) WaitForClient() {
	<-b.ready
}

func (b *broadcaster) WaitForClientCtx(ctx context.Context) bool {
	select {
	case <-b.ready:
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *broadcaster) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	b.once.Do(func() { close(b.ready) })
	return ch
}

func (b *broadcaster) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *broadcaster) publish(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- data:
		default: // slow client: drop
		}
	}
}

// ── sessions ─────────────────────────────────────────────────────────────────

type session struct {
	b      *broadcaster
	cancel context.CancelFunc
	done   chan struct{}
}

var sessions sync.Map // string → *session

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── MIDI parsing ─────────────────────────────────────────────────────────────

func toEvent(msg midi.Message) map[string]any {
	var ch, key, vel, pressure, program, controller, value, qframe, song uint8
	var relBend int16
	var absBend, spp uint16
	var bt []byte
	switch {
	case msg.GetNoteStart(&ch, &key, &vel):
		return map[string]any{"type": "note_on", "channel": ch, "key": key, "note": midi.Note(key).String(), "velocity": vel}
	case msg.GetNoteEnd(&ch, &key):
		return map[string]any{"type": "note_off", "channel": ch, "key": key, "note": midi.Note(key).String()}
	case msg.GetControlChange(&ch, &controller, &value):
		return map[string]any{"type": "cc", "channel": ch, "controller": controller, "value": value}
	case msg.GetPitchBend(&ch, &relBend, &absBend):
		return map[string]any{"type": "pitch", "channel": ch, "value": relBend}
	case msg.GetProgramChange(&ch, &program):
		return map[string]any{"type": "program", "channel": ch, "program": program}
	case msg.GetAfterTouch(&ch, &pressure):
		return map[string]any{"type": "aftertouch", "channel": ch, "pressure": pressure}
	case msg.GetPolyAfterTouch(&ch, &key, &pressure):
		return map[string]any{"type": "poly_aftertouch", "channel": ch, "key": key, "note": midi.Note(key).String(), "pressure": pressure}
	case msg.GetMTC(&qframe):
		return map[string]any{"type": "mtc", "quarter": qframe}
	case msg.GetSongSelect(&song):
		return map[string]any{"type": "song_select", "song": song}
	case msg.GetSPP(&spp):
		return map[string]any{"type": "spp", "position": spp}
	case msg.GetSysEx(&bt):
		return map[string]any{"type": "sysex", "data": bt}
	default:
		return map[string]any{"type": "unknown", "data": []byte(msg)}
	}
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

// sseHandler serves an SSE stream.
// With ?id=<session_id> it uses a per-session broadcaster;
// without ?id it uses the global broadcaster (--file / --port modes).
func sseHandler(globalB *broadcaster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		var b *broadcaster
		if id := r.URL.Query().Get("id"); id != "" {
			v, ok := sessions.Load(id)
			if !ok {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			b = v.(*session).b
		} else if globalB != nil {
			b = globalB
		} else {
			http.Error(w, "missing ?id=", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch := b.subscribe()
		defer b.unsubscribe(ch)

		slog.Debug("SSE client connected", "remote", r.RemoteAddr)
		for {
			select {
			case data := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-r.Context().Done():
				slog.Debug("SSE client disconnected", "remote", r.RemoteAddr)
				return
			}
		}
	}
}

// uploadHandler accepts POST /session with a multipart "file" field (.mid).
// Returns {"id": "<session_id>"} which the client uses for GET /events?id=...
func uploadHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "file too large (max 10 MB)", http.StatusBadRequest)
			return
		}

		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing 'file' field", http.StatusBadRequest)
			return
		}
		defer f.Close()

		data, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}

		var bpm float64
		fmt.Sscanf(r.FormValue("bpm"), "%f", &bpm)

		id := randomID()
		b := newBroadcaster()
		ctx, cancel := context.WithCancel(context.Background())
		sess := &session{b: b, cancel: cancel, done: make(chan struct{})}
		sessions.Store(id, sess)

		go func() {
			defer close(sess.done)
			defer sessions.Delete(id)
			defer cancel()

			// Wait up to 5 minutes for the browser to open the SSE stream.
			waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Minute)
			defer waitCancel()
			if !b.WaitForClientCtx(waitCtx) {
				slog.Debug("session: no client connected, cleaning up", "id", id)
				return
			}

			if err := runFileData(ctx, data, bpm, b); err != nil {
				slog.Error("playback error", "id", id, "err", err)
			}
			slog.Info("session finished", "id", id)
		}()

		slog.Info("session created", "id", id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}

// ── playback ──────────────────────────────────────────────────────────────────

func runFileData(ctx context.Context, data []byte, bpm float64, b *broadcaster) error {
	var events []smf.TrackEvent
	var fileBPM float64 = 120

	smf.ReadTracksFrom(bytes.NewReader(data)).Do(func(ev smf.TrackEvent) {
		var tempo float64
		if ev.Message.GetMetaTempo(&tempo) && fileBPM == 120 {
			fileBPM = tempo
		}
		if ev.Message.IsPlayable() {
			events = append(events, ev)
		}
	})

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].AbsMicroSeconds < events[j].AbsMicroSeconds
	})

	speed := 1.0
	if bpm > 0 {
		speed = fileBPM / bpm
	}

	slog.Info("playing", "file_bpm", fileBPM, "events", len(events))

	var elapsed int64
	for _, ev := range events {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		delay := ev.AbsMicroSeconds - elapsed
		if delay > 0 {
			select {
			case <-time.After(time.Duration(float64(delay)*speed) * time.Microsecond):
			case <-ctx.Done():
				return nil
			}
		}
		elapsed = ev.AbsMicroSeconds

		msg := midi.Message(ev.Message)
		event := toEvent(msg)
		eventData, _ := json.Marshal(event)
		slog.Debug("midi event", "event", event)
		b.publish(eventData)
	}
	return nil
}

func runFile(path string, bpm float64, b *broadcaster) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return runFileData(context.Background(), data, bpm, b)
}

func runPort(portName string, b *broadcaster) error {
	defer midi.CloseDriver()

	in, err := midi.FindInPort(portName)
	if err != nil {
		return fmt.Errorf("can't find port %q", portName)
	}

	slog.Info("listening on port", "port", portName)

	stop, err := midi.ListenTo(in, func(msg midi.Message, _ int32) {
		event := toEvent(msg)
		data, _ := json.Marshal(event)
		slog.Debug("midi event", "event", event)
		b.publish(data)
	}, midi.UseSysEx())
	if err != nil {
		return err
	}
	defer stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	return nil
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	file := flag.String("file", "", "path to .mid file (broadcast to all clients)")
	port := flag.String("port", "", "MIDI input port name (broadcast to all clients)")
	bpm := flag.Float64("bpm", 0, "override BPM (file mode only)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// globalB is only used in --file / --port modes.
	var globalB *broadcaster
	if *file != "" || *port != "" {
		globalB = newBroadcaster()
	}

	mux := http.NewServeMux()
	mux.Handle("/events", sseHandler(globalB))
	mux.Handle("/session", uploadHandler())

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		slog.Info("HTTP server starting", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "err", err)
			os.Exit(1)
		}
	}()

	host := *addr
	if len(host) > 0 && host[0] == ':' {
		host = "localhost" + host
	}

	var err error
	switch {
	case *file != "":
		slog.Info("waiting for browser to connect", "url", "http://"+host+"/events")
		globalB.WaitForClient()
		err = runFile(*file, *bpm, globalB)
	case *port != "":
		err = runPort(*port, globalB)
	default:
		slog.Info("upload mode — open frontend/index.html in your browser", "server", "http://"+host)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
	}

	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
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

// broadcaster fans out published events to all subscribed SSE clients.
type broadcaster struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	once    sync.Once
	ready   chan struct{} // closed when first client connects
}

// WaitForClient blocks until at least one SSE client has connected.
func (b *broadcaster) WaitForClient() {
	<-b.ready
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
		default:
			// slow client: drop
		}
	}
}

// toEvent converts a MIDI message into a JSON-serialisable map.
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

// sseHandler streams MIDI events to the browser via Server-Sent Events.
func sseHandler(b *broadcaster) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := b.subscribe()
		defer b.unsubscribe(ch)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

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

func runFile(path string, bpm float64, b *broadcaster) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var events []smf.TrackEvent
	var fileBPM float64 = 120

	smf.ReadTracksFrom(f).Do(func(ev smf.TrackEvent) {
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

	slog.Info("playing file", "path", path, "file_bpm", fileBPM, "events", len(events))

	var elapsed int64
	for _, ev := range events {
		delay := ev.AbsMicroSeconds - elapsed
		if delay > 0 {
			time.Sleep(time.Duration(float64(delay)*speed) * time.Microsecond)
		}
		elapsed = ev.AbsMicroSeconds

		msg := midi.Message(ev.Message)
		event := toEvent(msg)
		data, _ := json.Marshal(event)
		slog.Debug("midi event", "event", event)
		b.publish(data)
	}
	return nil
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

func main() {
	file := flag.String("file", "", "path to .mid file")
	port := flag.String("port", "", "MIDI input port name")
	bpm := flag.Float64("bpm", 0, "override BPM (file mode only)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	if *file == "" && *port == "" {
		fmt.Fprintln(os.Stderr, "usage: miviz --file <path.mid> [--bpm <bpm>] [--addr :8080]")
		fmt.Fprintln(os.Stderr, "       miviz --port <name> [--addr :8080]")
		os.Exit(1)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	b := &broadcaster{clients: make(map[chan []byte]struct{}), ready: make(chan struct{})}

	mux := http.NewServeMux()
	mux.Handle("/events", sseHandler(b))

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		slog.Info("HTTP server starting", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "err", err)
			os.Exit(1)
		}
	}()

	if *file != "" {
		slog.Info("waiting for browser to connect", "url", "http://"+*addr+"/events")
		b.WaitForClient()
	}

	var err error
	if *file != "" {
		err = runFile(*file, *bpm, b)
	} else {
		err = runPort(*port, b)
	}

	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

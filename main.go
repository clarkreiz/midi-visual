package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"gitlab.com/gomidi/midi/v2"
	_ "gitlab.com/gomidi/midi/v2/drivers/rtmididrv"
	"gitlab.com/gomidi/midi/v2/smf"
)

type Server struct {
	bpm     float64
	handler func(midi.Message)
}

func NewServer(bpm float64) *Server {
	return &Server{bpm: bpm, handler: printMessage}
}

func (s *Server) RunFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var events []smf.TrackEvent
	var fileBPM float64 = 120

	smf.ReadTracksFrom(f).Do(func(ev smf.TrackEvent) {
		var bpm float64
		if ev.Message.GetMetaTempo(&bpm) && fileBPM == 120 {
			fileBPM = bpm
		}
		if ev.Message.IsPlayable() {
			events = append(events, ev)
		}
	})

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].AbsMicroSeconds < events[j].AbsMicroSeconds
	})

	speed := 1.0
	if s.bpm > 0 {
		speed = fileBPM / s.bpm
	}

	var elapsed int64
	for _, ev := range events {
		delay := ev.AbsMicroSeconds - elapsed
		if delay > 0 {
			time.Sleep(time.Duration(float64(delay)*speed) * time.Microsecond)
		}
		elapsed = ev.AbsMicroSeconds
		s.handler(midi.Message(ev.Message))
	}
	return nil
}

func (s *Server) RunPort(portName string) error {
	defer midi.CloseDriver()

	in, err := midi.FindInPort(portName)
	if err != nil {
		return fmt.Errorf("can't find port %q", portName)
	}

	stop, err := midi.ListenTo(in, func(msg midi.Message, _ int32) {
		s.handler(msg)
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

func printMessage(msg midi.Message) {
	var event map[string]any
	var ch, key, vel, pressure, program, controller, value, qframe, song uint8
	var relBend int16
	var absBend, spp uint16
	var bt []byte
	switch {
	case msg.GetNoteStart(&ch, &key, &vel):
		event = map[string]any{"type": "note_on", "channel": ch, "key": key, "note": midi.Note(key).String(), "velocity": vel}
	case msg.GetNoteEnd(&ch, &key):
		event = map[string]any{"type": "note_off", "channel": ch, "key": key, "note": midi.Note(key).String()}
	case msg.GetControlChange(&ch, &controller, &value):
		event = map[string]any{"type": "cc", "channel": ch, "controller": controller, "value": value}
	case msg.GetPitchBend(&ch, &relBend, &absBend):
		event = map[string]any{"type": "pitch", "channel": ch, "value": relBend}
	case msg.GetProgramChange(&ch, &program):
		event = map[string]any{"type": "program", "channel": ch, "program": program}
	case msg.GetAfterTouch(&ch, &pressure):
		event = map[string]any{"type": "aftertouch", "channel": ch, "pressure": pressure}
	case msg.GetPolyAfterTouch(&ch, &key, &pressure):
		event = map[string]any{"type": "poly_aftertouch", "channel": ch, "key": key, "note": midi.Note(key).String(), "pressure": pressure}
	case msg.GetMTC(&qframe):
		event = map[string]any{"type": "mtc", "quarter": qframe}
	case msg.GetSongSelect(&song):
		event = map[string]any{"type": "song_select", "song": song}
	case msg.GetSPP(&spp):
		event = map[string]any{"type": "spp", "position": spp}
	case msg.GetSysEx(&bt):
		event = map[string]any{"type": "sysex", "data": bt}
	default:
		event = map[string]any{"type": "unknown", "data": []byte(msg)}
	}
	out, _ := json.Marshal(event)
	fmt.Println(string(out))
}

func main() {
	file := flag.String("file", "", "path to .mid file")
	port := flag.String("port", "", "MIDI input port name")
	bpm := flag.Float64("bpm", 0, "override BPM (file mode only)")
	flag.Parse()

	if *file == "" && *port == "" {
		fmt.Fprintln(os.Stderr, "usage: miviz --file <path.mid> [--bpm <bpm>]")
		fmt.Fprintln(os.Stderr, "       miviz --port <name>")
		os.Exit(1)
	}

	s := NewServer(*bpm)

	var err error
	if *file != "" {
		err = s.RunFile(*file)
	} else {
		err = s.RunPort(*port)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(1)
	}
}

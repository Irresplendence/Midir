package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/Marcentus/Midir/packet"
)

var mbe = binary.BigEndian
var mle = binary.LittleEndian

// stripExitLagForTest is a minimal copy of the private exitLagPacketLoop framing
// logic in packet/gamePacketReader.go, adapted to operate on a single in-memory
// buffer instead of a channel pipeline, purely so this test can feed real
// captured bytes through the real ParseGamePacket/ParsePropXxxPacket code.
func stripExitLagForTest(buf *bytes.Buffer) [][]byte {
	var out [][]byte
	isReadingMabiPayload := false
	var mabiBytesToRead uint32

	for {
		if !isReadingMabiPayload {
			found := false
			for buf.Len() >= 5 {
				bb := buf.Bytes()
				if bb[0] == 0x01 && bb[4] == 0x05 {
					found = true
					break
				}
				buf.Next(1)
			}
			if !found {
				break
			}
		}

		if isReadingMabiPayload {
			if uint32(buf.Len()) < mabiBytesToRead {
				break
			}
			payload := make([]byte, mabiBytesToRead)
			buf.Read(payload)
			if len(payload) == 5 && string(payload) == "pong\n" {
				if buf.Len() >= 4 && bytes.Equal(buf.Bytes()[:4], []byte{0x05, 0x25, 0x01, 0x01}) {
					buf.Next(4)
				}
				isReadingMabiPayload = false
				continue
			}
			out = append(out, payload)
			if buf.Len() >= 4 && bytes.Equal(buf.Bytes()[:4], []byte{0x05, 0x25, 0x01, 0x01}) {
				buf.Next(4)
			}
			isReadingMabiPayload = false
			continue
		}

		b := buf.Bytes()
		if len(b) < 38 {
			break
		}
		seqLenIndOffset := 1 + 2 + 30
		seqLenInd := b[seqLenIndOffset]

		if seqLenInd == 0 {
			bodyLen := mle.Uint16(b[1:3])
			totalSize := 1 + 2 + int(bodyLen)
			if buf.Len() < totalSize {
				break
			}
			if buf.Len() >= totalSize+4 && bytes.Equal(b[totalSize:totalSize+4], []byte{0x05, 0x25, 0x01, 0x01}) {
				buf.Next(totalSize + 4)
			} else {
				buf.Next(totalSize)
			}
			continue
		}

		seqLen := int(seqLenInd)
		if seqLen <= 0 || seqLen > 8 {
			buf.Next(1)
			continue
		}
		payloadLenIndOffset := seqLenIndOffset + 1 + seqLen
		if buf.Len() < payloadLenIndOffset+1 {
			break
		}
		payloadLenInd := b[payloadLenIndOffset]
		var mabiPayloadLenBytes int
		if payloadLenInd == 0x05 {
			mabiPayloadLenBytes = 1
		} else if payloadLenInd == 0x09 {
			mabiPayloadLenBytes = 2
		} else {
			buf.Next(1)
			continue
		}
		flagOffset := payloadLenIndOffset + 1
		if buf.Len() < flagOffset+1 {
			break
		}
		if b[flagOffset] != 0x23 {
			buf.Next(1)
			continue
		}
		mabiLenOffset := flagOffset + 1
		if buf.Len() < mabiLenOffset+mabiPayloadLenBytes {
			break
		}
		var mabiLen uint32
		if mabiPayloadLenBytes == 1 {
			mabiLen = uint32(b[mabiLenOffset])
		} else {
			mabiLen = uint32(mle.Uint16(b[mabiLenOffset : mabiLenOffset+2]))
		}
		headerTotalLen := mabiLenOffset + mabiPayloadLenBytes
		buf.Next(headerTotalLen)
		isReadingMabiPayload = true
		mabiBytesToRead = mabiLen
	}
	return out
}

func loadHexFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	data, err := hex.DecodeString(string(bytes.TrimSpace(raw)))
	if err != nil {
		t.Fatalf("decode hex %s: %v", path, err)
	}
	return data
}

// feedFrame strips the ExitLag envelope from a raw captured TCP payload and
// runs every resulting GamePacket through the FarmTracker via handleFarmPropPacket's
// real dispatch logic, returning any farm_prop events produced.
func feedFrame(t *testing.T, ft *FarmTracker, data []byte) []*farmPropData {
	t.Helper()
	var results []*farmPropData

	buf := bytes.NewBuffer(data)
	for _, mp := range stripExitLagForTest(buf) {
		pb := bytes.NewBuffer(mp)
		for pb.Len() > 0 {
			gp, err := packet.ParseGamePacket(pb, time.Now())
			if err != nil {
				break
			}
			if gp.IsShortPacket {
				continue
			}
			switch gp.Op {
			case opcodePropAppear:
				info, err := packet.ParsePropAppearPacket(gp)
				if err == nil {
					ft.HandlePropAppear(info)
				}
			case opcodePropUpdate:
				info, err := packet.ParsePropUpdatePacket(gp)
				if err == nil {
					if d := ft.HandlePropUpdate(info); d != nil {
						results = append(results, d)
					}
				}
			case opcodePropDisappear:
				info, err := packet.ParsePropDisappearPacket(gp)
				if err == nil {
					if d := ft.HandlePropDisappear(info.Id, info.LinkId, info.At); d != nil {
						results = append(results, d)
					}
				}
			case opcodeFarmSystemMessage:
				name, ok, err := packet.ParseFarmSystemMessage(gp)
				if err == nil && ok {
					ft.HandlePlantMessage(name, gp.At)
				}
			}
		}
	}
	return results
}

func TestFarmTrackerAgainstRealCapture(t *testing.T) {
	ft := NewFarmTracker()

	plantEvents := feedFrame(t, ft, loadHexFile(t, "testdata/plant_burst.hex"))
	t.Logf("plant burst frame -> %d farm_prop event(s)", len(plantEvents))
	for _, e := range plantEvents {
		t.Logf("  %+v", *e)
	}

	tendEvents := feedFrame(t, ft, loadHexFile(t, "testdata/tend_event.hex"))
	t.Logf("tend frame -> %d farm_prop event(s)", len(tendEvents))
	for _, e := range tendEvents {
		t.Logf("  %+v", *e)
	}
}

// TestFarmTrackerHarvest validates the PropDisappear path against a real
// captured harvest (stream1_concat.hex: a "single"-tagged PropUpdate with
// linkprop cleared, immediately followed by the real PropDisappear packet).
// The isolated 90s harvest capture didn't span back far enough to observe this
// plot's original plant-time "seed" PropUpdate (which is what normally
// registers the seed id -> fieldprop link in continuous operation), so that
// link is primed synthetically here to reproduce what a long-running tracker
// would already know by harvest time.
func TestFarmTrackerHarvest(t *testing.T) {
	ft := NewFarmTracker()

	const fieldprop = 45468035624468482
	const seedLinkId = 45468035624468502

	// Simulate the plant-time "seed" PropUpdate a continuously-running tracker
	// would have already seen, establishing seedLinkId -> fieldprop.
	primed := ft.HandlePropUpdate(&packet.PropUpdateInfo{
		Id:  seedLinkId,
		Tag: "seed",
		XML: packet.PropXMLAttrs{
			Owner:        4503599629455493,
			HasFieldProp: true,
			FieldProp:    fieldprop,
			HasItemId:    true,
			ItemId:       5041232,
		},
	})
	if primed == nil {
		t.Fatalf("priming update unexpectedly produced no plant event")
	}
	t.Logf("primed: %+v", *primed)

	harvestEvents := feedFrame(t, ft, loadHexFile(t, "testdata/harvest_window.hex"))
	t.Logf("harvest window -> %d farm_prop event(s)", len(harvestEvents))
	for _, e := range harvestEvents {
		t.Logf("  %+v", *e)
	}

	found := false
	for _, e := range harvestEvents {
		if e.Event == "harvest" && e.FieldProp == "45468035624468482" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a harvest event for fieldprop %d, got %+v", fieldprop, harvestEvents)
	}
}

// TestFarmTrackerFullLifecycle replays three narrow capture windows - taken
// from a single continuous ~14 minute live capture of one plot's entire
// plant -> ready -> harvest cycle, trimmed to just the frames surrounding
// each event to exclude unrelated traffic (other plots/players/chat) that
// happened to be interleaved in the full stream. Confirms the tracker emits
// exactly plant, then ready, then harvest for that plot, in that order.
//
// Real-world timestamps from the original full capture (not preserved by
// these trimmed fixtures) showed plant->ready took 12m02s, matching the
// in-game countdown shown at plant time almost exactly, and ready->harvest
// took 53s (the player noticing and walking over).
func TestFarmTrackerFullLifecycle(t *testing.T) {
	ft := NewFarmTracker()

	const targetField = "45467842350940162"

	var events []*farmPropData
	events = append(events, feedFrame(t, ft, loadHexFile(t, "testdata/lifecycle_plant.hex"))...)
	events = append(events, feedFrame(t, ft, loadHexFile(t, "testdata/lifecycle_ready.hex"))...)
	events = append(events, feedFrame(t, ft, loadHexFile(t, "testdata/lifecycle_harvest.hex"))...)

	var sequence []string
	var matched []*farmPropData
	for _, e := range events {
		if e.FieldProp == targetField {
			sequence = append(sequence, e.Event)
			matched = append(matched, e)
			t.Logf("  %+v", *e)
		}
	}

	want := []string{"plant", "ready", "harvest"}
	if len(sequence) != len(want) {
		t.Fatalf("expected event sequence %v for fieldprop %s, got %v", want, targetField, sequence)
	}
	for i, w := range want {
		if sequence[i] != w {
			t.Errorf("event %d: expected %q, got %q (full sequence: %v)", i, w, sequence[i], sequence)
		}
	}

	// Name should be correlated from the plant-time "You used Blackberry
	// Seeds (Taillteann Farm)!" system message, which rides in the same
	// packet burst as the plant update, and should stick on every subsequent
	// event for this plot (ready, harvest), not just the plant event itself.
	for i, e := range matched {
		if e.Name != "Blackberry Seeds" {
			t.Errorf("event %d (%s): expected Name %q, got %q", i, e.Event, "Blackberry Seeds", e.Name)
		}
		if e.At <= 0 {
			t.Errorf("event %d (%s): expected a positive unix timestamp, got %d", i, e.Event, e.At)
		}
	}
}

// TestFarmTrackerRenewableNodeHarvest validates the "collecting"-tag harvest
// path for renewable nodes (Tree/Quartz/Spider), which never fire
// PropDisappears the way crop plots do - confirmed by a real capture showing
// zero PropDisappears packets across four back-to-back harvests. The fixture
// is a real captured Spider Web harvest: "collecting" tag, then the "Common
// Magic Cobweb...placed in storage" confirmation, then the node resetting to
// "empty" - all for fieldprop 45467842350940167.
func TestFarmTrackerRenewableNodeHarvest(t *testing.T) {
	ft := NewFarmTracker()

	const fieldprop = 45467842350940167

	// Prime with the plant this node would have already gone through in
	// continuous operation (this capture starts mid-lifecycle, same situation
	// as TestFarmTrackerHarvest above).
	primed := ft.HandlePropUpdate(&packet.PropUpdateInfo{
		Id:  fieldprop,
		Tag: "empty",
		XML: packet.PropXMLAttrs{
			Owner:        4503599629455493,
			HasFieldProp: true,
			FieldProp:    fieldprop,
			HasItemId:    true,
			ItemId:       5041237,
		},
	})
	if primed == nil || primed.Event != "plant" {
		t.Fatalf("priming update unexpectedly did not produce a plant event: %+v", primed)
	}

	events := feedFrame(t, ft, loadHexFile(t, "testdata/renewable_harvest.hex"))

	var sequence []string
	for _, e := range events {
		if e.FieldProp == "45467842350940167" {
			sequence = append(sequence, e.Event)
			t.Logf("  %+v", *e)
		}
	}

	if len(sequence) != 1 || sequence[0] != "harvest" {
		t.Fatalf("expected exactly one harvest event, got %v", sequence)
	}

	// resetPlotForNextCycle only clears harvestEmitted (not plantEmitted), so a
	// later "collecting" on the same fieldprop should fire harvest again -
	// renewable nodes can be collected from repeatedly without a full replant.
	again := ft.HandlePropUpdate(&packet.PropUpdateInfo{
		Id:  fieldprop,
		Tag: "collecting",
		XML: packet.PropXMLAttrs{Owner: 4503599629455493, HasFieldProp: true, FieldProp: fieldprop, HasItemId: true, ItemId: 5041237},
	})
	if again == nil || again.Event != "harvest" {
		t.Errorf("expected a second harvest event on a later \"collecting\", got %+v", again)
	}
}

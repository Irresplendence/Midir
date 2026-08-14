package packet

import (
	"fmt"
	"regexp"
)

var plantMessageRe = regexp.MustCompile(`^You used (.+?) \([^)]*\)!`)

// ParseFarmSystemMessage parses a generic system message packet (0x526d) and,
// if it matches the plant-confirmation shape ("You used <item> (<farm>)!..."),
// returns the planted item's display name (e.g. "Blackberry Seeds",
// "Rubber Tree Nutrients"). This opcode is shared by several unrelated
// message types (e.g. "Your tending succeeded!"), so ok=false is the expected,
// normal result for anything that isn't a plant confirmation - callers should
// not treat it as an error.
func ParseFarmSystemMessage(p *GamePacket) (name string, ok bool, err error) {
	msg := p.Msg
	if len(msg) < 2 {
		return "", false, fmt.Errorf("system message packet too short")
	}
	if msg[1].Type() != MessageElemTypeString {
		return "", false, fmt.Errorf("system message text has unexpected type %v", msg[1].Type())
	}

	text := msg[1].Data().(string)
	m := plantMessageRe.FindStringSubmatch(text)
	if m == nil {
		return "", false, nil
	}
	return m[1], true, nil
}

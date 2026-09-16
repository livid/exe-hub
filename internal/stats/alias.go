package stats

import "encoding/hex"

// Alias is a visitor's name on the Live list — a colour and an animal
// chosen by the visitor id, so one person reads as one name for the
// day and no id is printed. 20 × 40 names; a collision is a shared
// name, nothing more.
func Alias(vid string) string {
	b, err := hex.DecodeString(vid)
	if err != nil || len(b) < 2 {
		return "Visitor"
	}
	return colours[int(b[0])%len(colours)] + " " + animals[int(b[1])%len(animals)]
}

var colours = []string{"Amber", "Aqua", "Azure", "Cobalt", "Coral", "Crimson", "Gold", "Indigo", "Ivory", "Jade",
	"Lilac", "Olive", "Peach", "Pearl", "Ruby", "Sage", "Silver", "Slate", "Teal", "Violet"}

var animals = []string{"Badger", "Bear", "Camel", "Crab", "Crane", "Deer", "Dove", "Falcon", "Fox", "Goat",
	"Hare", "Heron", "Koala", "Lark", "Llama", "Lynx", "Mole", "Moose", "Newt", "Otter",
	"Owl", "Panda", "Parrot", "Penguin", "Quail", "Rabbit", "Raven", "Seal", "Squid", "Stork",
	"Swan", "Tiger", "Toad", "Turtle", "Whale", "Wolf", "Wren", "Yak", "Zebra", "Ibis"}

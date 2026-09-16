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

// Colour is the alias's colour as a CSS hex, for the disc the Live
// list draws before the name (asked by Livid 2026-09-16: the name is a
// colour, so show it).
func Colour(vid string) string {
	b, err := hex.DecodeString(vid)
	if err != nil || len(b) < 1 {
		return "#ccc"
	}
	return hexes[int(b[0])%len(colours)]
}

var colours = []string{"Amber", "Aqua", "Azure", "Cobalt", "Coral", "Crimson", "Gold", "Indigo", "Ivory", "Jade",
	"Lilac", "Olive", "Peach", "Pearl", "Ruby", "Sage", "Silver", "Slate", "Teal", "Violet"}

// hexes are the colours by name, in the same order.
var hexes = []string{"#ff9900", "#33cccc", "#3399ff", "#0047ab", "#ff7f50", "#dc143c", "#ffd700", "#4b0082", "#fffff0", "#00a86b",
	"#c8a2c8", "#808000", "#ffcba4", "#eae0c8", "#9b111e", "#9caf88", "#c0c0c0", "#708090", "#008080", "#8f00ff"}

var animals = []string{"Badger", "Bear", "Camel", "Crab", "Crane", "Deer", "Dove", "Falcon", "Fox", "Goat",
	"Hare", "Heron", "Koala", "Lark", "Llama", "Lynx", "Mole", "Moose", "Newt", "Otter",
	"Owl", "Panda", "Parrot", "Penguin", "Quail", "Rabbit", "Raven", "Seal", "Squid", "Stork",
	"Swan", "Tiger", "Toad", "Turtle", "Whale", "Wolf", "Wren", "Yak", "Zebra", "Ibis"}

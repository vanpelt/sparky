package sshgw

import "crypto/rand"

// Playful "adjective-noun" sandbox names, Docker/Heroku style (e.g.
// "swift-otter"). All words are lowercase single tokens so a generated name is
// DNS-safe and doubles as both the SSH username and the proxy subdomain
// (swift-otter.<domain>). Collisions are handled by the caller.

var nameAdjectives = []string{
	"amber", "brave", "breezy", "bubbly", "clever", "cosmic", "cozy", "crafty",
	"dapper", "dazzling", "eager", "electric", "feisty", "fluffy", "fuzzy",
	"gentle", "giddy", "glowing", "jazzy", "jolly", "lively", "lucky", "mellow",
	"merry", "mighty", "nifty", "nimble", "peppy", "plucky", "quirky", "radiant",
	"rowdy", "sassy", "sleepy", "snappy", "spry", "sturdy", "sunny", "swift",
	"tidy", "turbo", "velvet", "whimsical", "witty", "wobbly", "zany", "zesty",
	"zippy",
}

var nameNouns = []string{
	"axolotl", "aurora", "badger", "boulder", "canyon", "capybara", "cedar",
	"comet", "cosmos", "dingo", "falcon", "fern", "ferret", "gecko", "glacier",
	"harbor", "hedgehog", "heron", "koala", "lagoon", "lemur", "lynx", "magpie",
	"maple", "marmot", "meerkat", "meteor", "mongoose", "narwhal", "nebula",
	"nova", "ocelot", "otter", "panda", "pangolin", "pebble", "puffin", "pulsar",
	"quasar", "quokka", "raven", "tapir", "thicket", "walrus", "willow", "wombat",
	"wren", "zephyr",
}

// randomName returns an "adjective-noun" name. It is not guaranteed unique;
// callers should check against existing sandboxes and retry (see newName).
func randomName() string {
	return nameAdjectives[randIndex(len(nameAdjectives))] + "-" +
		nameNouns[randIndex(len(nameNouns))]
}

// randIndex returns a uniform-ish index in [0,n) using crypto/rand. The modulo
// bias is negligible for these small word lists.
func randIndex(n int) int {
	b := make([]byte, 2)
	rand.Read(b) //nolint:errcheck
	return int(uint16(b[0])<<8|uint16(b[1])) % n
}

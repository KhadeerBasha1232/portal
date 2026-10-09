package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// A code looks like "8-maple-otter".
//
//   - The number is the "nameplate". It is public: both sides use it to find
//     each other on the network (mDNS on the LAN, the libp2p DHT on the internet).
//   - The whole code is the PAKE password. The words never leave the machine;
//     they only feed SPAKE2. Two words from 256 = 16 bits, which is plenty
//     because an attacker gets one online guess per attempt (and the sharer
//     stops after maxBadAttempts).
const maxNameplate = 999

func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(maxNameplate))
	if err != nil {
		return "", err
	}
	parts := []string{strconv.FormatInt(n.Int64()+1, 10)}
	for range 2 {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
		if err != nil {
			return "", err
		}
		parts = append(parts, words[i.Int64()])
	}
	return strings.Join(parts, "-"), nil
}

// parseCode normalizes a code and returns its nameplate.
func parseCode(code string) (normalized, nameplate string, err error) {
	code = strings.ToLower(strings.TrimSpace(code))
	plate, rest, ok := strings.Cut(code, "-")
	if !ok || rest == "" {
		return "", "", errors.New("code must look like 8-maple-otter")
	}
	n, err := strconv.Atoi(plate)
	if err != nil || n < 1 || n > maxNameplate {
		return "", "", fmt.Errorf("code must start with a number from 1 to %d", maxNameplate)
	}
	return code, strconv.Itoa(n), nil
}

var words = [256]string{
	"apple", "arrow", "atlas", "badge", "bacon", "banjo", "basil", "beach", "bison", "blade", "bloom", "board", "bonus", "brain", "bread", "brick",
	"bridge", "brook", "brush", "cabin", "cable", "cactus", "camel", "candy", "canoe", "cargo", "carrot", "castle", "cedar", "chalk", "cherry", "chess",
	"chief", "cider", "cliff", "clock", "cloud", "clover", "cobra", "comet", "coral", "cotton", "crane", "crown", "curry", "daisy", "delta", "denim",
	"desert", "diary", "dingo", "disco", "dolphin", "donut", "dragon", "drum", "eagle", "easel", "echo", "elbow", "ember", "engine", "falcon", "fern",
	"ferry", "fiber", "flame", "flute", "forest", "fossil", "fox", "frost", "galaxy", "garden", "garlic", "gecko", "ghost", "ginger", "giraffe", "glacier",
	"globe", "goat", "grape", "gravel", "guitar", "hammer", "harbor", "hazel", "helmet", "heron", "hippo", "honey", "hornet", "hotel", "igloo", "island",
	"ivory", "jacket", "jaguar", "jelly", "jewel", "jungle", "kayak", "kettle", "kiwi", "koala", "ladder", "lagoon", "lamp", "lantern", "lemon", "lily",
	"lion", "lizard", "lobster", "locket", "lotus", "magnet", "mango", "maple", "marble", "meadow", "melon", "mint", "mirror", "moose", "motor", "muffin",
	"nectar", "needle", "nickel", "noodle", "nutmeg", "oasis", "ocean", "olive", "onion", "orbit", "orchid", "otter", "owl", "oyster", "paddle", "panda",
	"panther", "paper", "parrot", "pasta", "peach", "peanut", "pebble", "pepper", "piano", "pickle", "pigeon", "pillow", "pilot", "pine", "pirate", "pizza",
	"planet", "plum", "pocket", "polar", "pony", "poppy", "potato", "prism", "pumpkin", "puzzle", "quartz", "quill", "rabbit", "radar", "radio", "raven",
	"reef", "rhino", "ribbon", "river", "robin", "rocket", "rose", "ruby", "saddle", "salmon", "sandal", "satin", "scarf", "shark", "shell", "sierra",
	"silver", "skate", "sketch", "sloth", "snail", "socket", "spark", "spider", "spoon", "squid", "star", "stone", "storm", "sugar", "summit", "sunset",
	"swan", "tablet", "taco", "tango", "temple", "tiger", "timber", "toast", "tomato", "topaz", "torch", "tractor", "tulip", "tundra", "turtle", "tuxedo",
	"umbrella", "unicorn", "valley", "vanilla", "velvet", "violet", "viper", "volcano", "waffle", "walnut", "walrus", "wave", "whale", "willow", "window", "wizard",
	"wolf", "yacht", "yarn", "yeti", "yogurt", "zebra", "zenith", "zipper", "acorn", "anchor", "beetle", "bucket", "candle", "cobalt", "copper", "cricket",
}

func init() {
	seen := map[string]bool{}
	for _, w := range words {
		if w == "" || seen[w] {
			panic("wordlist must have 256 unique words, bad entry: " + strconv.Quote(w))
		}
		seen[w] = true
	}
}

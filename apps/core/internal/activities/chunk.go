package activities

import (
	"strings"
	"unicode/utf8"
)

// Chunk splits text into overlapping chunks of at most maxChunkSize runes.
func chunk(text string, maxChunkSize, overlap int) []string {
	if maxChunkSize <= 0 {
		maxChunkSize = 1000
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= maxChunkSize {
		overlap = maxChunkSize / 4
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var chunks []string
	start := 0
	runeLen := utf8.RuneCountInString(text)
	for start < runeLen {
		end := start + maxChunkSize
		if end > runeLen {
			end = runeLen
		}
		chunk := string([]rune(text)[start:end])
		chunks = append(chunks, strings.TrimSpace(chunk))
		if end == runeLen {
			break
		}
		start = end - overlap
		if start < 0 {
			start = 0
		}
	}
	return chunks
}

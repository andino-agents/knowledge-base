// Package chunk splits extracted documents into retrieval-sized pieces.
package chunk

import (
	"strings"

	"github.com/andino-agents/knowledge-base/internal/pipeline/extract"
	"github.com/andino-agents/knowledge-base/internal/store"
)

// TokenEstimator approximates the token count of a text. v0.1 ships the
// chars/4 heuristic; a real tokenizer can plug in behind this.
type TokenEstimator func(text string) int

// CharsPerToken is the conservative default estimate for latin-ish text.
const CharsPerToken = 4

// Estimate is the default TokenEstimator.
func Estimate(text string) int {
	n := len(text) / CharsPerToken
	if n == 0 && len(text) > 0 {
		n = 1
	}
	return n
}

// Split turns a document's blocks into chunks of at most maxTokens
// (estimated), splitting oversized blocks by lines with overlapTokens of
// trailing context carried into the next chunk. Block boundaries (markdown
// sections) are never merged: a chunk always belongs to one heading path.
func Split(doc extract.Doc, maxTokens, overlapTokens int, est TokenEstimator) []store.Chunk {
	if est == nil {
		est = Estimate
	}
	if maxTokens <= 0 {
		maxTokens = 512
	}
	if overlapTokens < 0 {
		overlapTokens = 0
	}

	var chunks []store.Chunk
	seq := 0
	add := func(text, headingPath string, startLine, endLine int) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		chunks = append(chunks, store.Chunk{
			Seq:         seq,
			HeadingPath: headingPath,
			StartLine:   startLine,
			EndLine:     endLine,
			Text:        text,
			TokenEst:    est(text),
		})
		seq++
	}

	for _, b := range doc.Blocks {
		if est(b.Text) <= maxTokens {
			add(b.Text, b.HeadingPath, b.StartLine, b.EndLine)
			continue
		}
		splitBlock(b, maxTokens, overlapTokens, est, add)
	}
	return chunks
}

// splitBlock cuts an oversized block line by line. Overlap is measured in
// estimated tokens of trailing lines repeated at the start of the next chunk.
func splitBlock(b extract.Block, maxTokens, overlapTokens int, est TokenEstimator, add func(text, hp string, s, e int)) {
	lines := strings.Split(b.Text, "\n")

	var (
		cur      []string
		curStart = b.StartLine
		curTok   int
		fresh    bool // has cur gained lines since the last flush?
	)
	flush := func(endLine int) {
		if len(cur) == 0 {
			return
		}
		fresh = false
		add(strings.Join(cur, "\n"), b.HeadingPath, curStart, endLine)
		// Carry trailing lines as overlap into the next chunk.
		var (
			keep    []string
			keepTok int
		)
		for i := len(cur) - 1; i >= 0 && keepTok < overlapTokens; i-- {
			keep = append([]string{cur[i]}, keep...)
			keepTok += est(cur[i])
		}
		curStart = endLine - len(keep) + 1
		cur = keep
		curTok = keepTok
	}

	for i, line := range lines {
		lineNo := b.StartLine + i
		lineTok := est(line)
		// A single line larger than maxTokens still becomes its own chunk;
		// we never split inside a line.
		if curTok+lineTok > maxTokens && len(cur) > 0 {
			flush(lineNo - 1)
		}
		cur = append(cur, line)
		curTok += lineTok
		fresh = true
	}
	if fresh {
		add(strings.Join(cur, "\n"), b.HeadingPath, curStart, b.EndLine)
	}
}

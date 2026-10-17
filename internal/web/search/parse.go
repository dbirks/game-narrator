package search

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// parseExaToolText converts the text content of an Exa tools/call result
// into Result values. The tool returns text (usually a JSON array of hits);
// parsing is defensive: direct JSON parse, then a JSON-array substring,
// then the raw text kept as a single fallback snippet. Result URLs are
// untrusted: only absolute http/https URLs survive, and every caller must
// still re-validate them with the web fetcher policy before fetching.
func parseExaToolText(source, tool, text string, max int, now time.Time) ([]Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("search: %s returned empty content", tool)
	}
	if max <= 0 {
		max = DefaultMaxResults
	}
	if hits, ok := extractJSONArrayOfObjects(trimmed); ok {
		var results []Result
		for _, hit := range hits {
			title := jsonStringField(hit, "title")
			rawURL := jsonStringField(hit, "url")
			if !isSafeResultURL(rawURL) {
				continue
			}
			snippet := ""
			for _, key := range []string{"text", "snippet", "description", "content", "summary"} {
				if v := jsonStringField(hit, key); v != "" {
					snippet = v
					break
				}
			}
			results = append(results, Result{
				Title:       title,
				URL:         rawURL,
				Snippet:     capSnippet(snippet),
				Source:      source,
				RetrievedAt: now,
			})
			if len(results) >= max {
				break
			}
		}
		if len(results) > 0 {
			return results, nil
		}
		// Parsed array but no usable http(s) URLs: fall through so the
		// raw text is preserved rather than silently dropped.
	}
	// The tool may also answer in a human-readable format, one block per
	// hit: "Title: … URL: https://… Published: … …". Parse that shape
	// before giving up to the raw-snippet fallback.
	if blockResults, ok := parseExaTextBlocks(trimmed); ok {
		if len(blockResults) > max {
			blockResults = blockResults[:max]
		}
		return blockResults, nil
	}
	// Last resort: the text has no recognizable structure but contains
	// absolute URLs — surface them (full text stays in the snippet).
	if urls := extractAllURLs(trimmed); len(urls) > 0 {
		results := make([]Result, 0, len(urls))
		for _, u := range urls {
			results = append(results, Result{
				Title:       tool,
				URL:         u,
				Snippet:     capSnippet(trimmed),
				Source:      source,
				RetrievedAt: now,
			})
			if len(results) >= max {
				break
			}
		}
		return results, nil
	}
	return []Result{{
		Title:       tool,
		URL:         "",
		Snippet:     capSnippet(trimmed),
		Source:      source,
		RetrievedAt: now,
	}}, nil
}

// exaBlockURLRe finds the start of one "URL: <absolute http(s) URL>" line
// in Exa's human-readable text format.
var exaBlockURLRe = regexp.MustCompile(`(?m)^URL:\s*(https?://\S+)\s*$`)

// parseExaTextBlocks parses Exa's text-mode tool output: one block per hit
// starting with a "URL: https://…" line, preceded by "Title: …" and
// followed by Published/Author/Highlights/Summary lines. Returns false
// when no URL lines are present (caller falls back to the raw snippet).
func parseExaTextBlocks(text string) ([]Result, bool) {
	matches := exaBlockURLRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, false
	}
	now := time.Now().UTC()
	var results []Result
	for i, m := range matches {
		url := text[m[2]:m[3]]
		start := 0
		if i > 0 {
			start = matches[i-1][3]
		}
		block := text[start:m[0]]
		title := ""
		if j := strings.Index(block, "Title:"); j >= 0 {
			rest := block[j+len("Title:"):]
			if k := strings.IndexByte(rest, '\n'); k >= 0 {
				rest = rest[:k]
			}
			title = strings.TrimSpace(rest)
		}
		after := text[m[1]:]
		cut := len(after)
		if k := strings.Index(after, "\nTitle:"); k >= 0 && k < cut {
			cut = k
		}
		if k := strings.Index(after, "\nURL:"); k >= 0 && k < cut {
			cut = k
		}
		snippet := strings.TrimSpace(after[:cut])
		results = append(results, Result{
			Title:       title,
			URL:         url,
			Snippet:     capSnippet(snippet),
			Source:      "exa-mcp",
			RetrievedAt: now,
		})
	}
	return results, true
}

// exaURLRe matches absolute http(s) URLs in free text (last-resort
// extraction when the tool text is unstructured).
var exaURLRe = regexp.MustCompile(`https?://[^\s"'\\)\]]+`)

// urlTrimSet trims trailing punctuation that free-text URL extraction
// picks up (a raw string because it contains a quote character).
const urlTrimSet = `.,;:)]"`

// extractAllURLs returns up to 20 unique absolute http(s) URLs found in
// free text, in order of first appearance.
func extractAllURLs(text string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range exaURLRe.FindAllString(text, -1) {
		u := strings.TrimRight(m, urlTrimSet)
		if !isSafeResultURL(u) || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

// extractJSONArrayOfObjects parses text as a JSON array of objects, or
// extracts the first top-level-looking JSON array inside it.
func extractJSONArrayOfObjects(text string) ([]map[string]any, bool) {
	if arr, ok := tryObjectArray(text); ok {
		return arr, true
	}
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start >= 0 && end > start {
		return tryObjectArray(text[start : end+1])
	}
	return nil, false
}

func tryObjectArray(text string) ([]map[string]any, bool) {
	var arr []map[string]any
	if json.Unmarshal([]byte(text), &arr) != nil {
		return nil, false
	}
	return arr, true
}

// jsonStringField returns the value of field as a trimmed string when it
// is a JSON string; anything else yields "".
func jsonStringField(hit map[string]any, field string) string {
	v, ok := hit[field]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// capSnippet bounds a single snippet so search results stay small inside
// the agent loop (full text belongs to fetch_page, not web_search).
func capSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= exaMCPMaxSnippet {
		return s
	}
	return s[:exaMCPMaxSnippet] + "…"
}

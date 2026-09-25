package search

// Snippet is the one-line window `search` prints for a chunk, for a caller
// that renders hits itself.
func (r Render) Snippet(text, query string) string { return r.snippet(text, query) }

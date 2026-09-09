package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type Vars struct {
	ID         string
	Title      string
	Uploader   string
	UploadDate string // YYYYMMDD as the extractor reports it
}

// maxSegment caps one path segment at 120 runes rather than 255 bytes. A
// non-ASCII title costs two bytes per character and, together with a long
// parent directory, easily hits the 260 character path limit on windows.
const maxSegment = 120

// titleMark holds the title's place until its budget is known.
const titleMark = "\x00title\x00"

// RenderLayout expands a layout template into a path without an extension.
// Placeholders: {id}, {title}, {uploader}, {upload_date}, {upload_year},
// {upload_month}, {upload_day}.
//
// The title is substituted last and on its own. Trimming an assembled
// segment from the right would cut off the trailing "[id]", and the id is
// the archive key: it is what lookups and cross-checks rely on.
func RenderLayout(tpl string, v Vars) string {
	year, month, day := splitDate(v.UploadDate)
	repl := strings.NewReplacer(
		"{id}", SanitizeSegment(v.ID),
		"{title}", titleMark,
		"{uploader}", SanitizeSegment(orUnknown(v.Uploader)),
		"{upload_date}", SanitizeSegment(v.UploadDate),
		"{upload_year}", year,
		"{upload_month}", month,
		"{upload_day}", day,
	)
	// values are cleaned before the split, not after: a channel called
	// "Rock/Pop" would otherwise inject a directory level of its own. The
	// title is exempt because it travels as titleMark and is inserted into an
	// already split segment.
	raw := repl.Replace(tpl)

	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == '\\' })
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := SanitizeSegment(fitTitle(p, v.Title)); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return SanitizeSegment(v.ID)
	}
	return strings.Join(clean, "/")
}

// fitTitle inserts the title, trimmed to whatever budget the segment has
// left. If the rest of the template does not fit on its own the title gets
// nothing, which is correct: the final trim would cut it off anyway and take
// whatever followed it with it.
func fitTitle(segment, title string) string {
	if !strings.Contains(segment, titleMark) {
		return segment
	}
	fixed := utf8.RuneCountInString(strings.ReplaceAll(segment, titleMark, ""))
	budget := max(maxSegment-fixed, 0)
	return strings.ReplaceAll(segment, titleMark, trimRunes(title, budget))
}

// orUnknown keeps a missing value from collapsing a level of the tree: one
// video landing in channel/year and another straight in year would split the
// archive into two shapes.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func splitDate(d string) (year, month, day string) {
	if len(d) < 8 {
		return "unknown", "00", "00"
	}
	return d[0:4], d[4:6], d[6:8]
}

// reservedNames are names windows refuses to use for a file in any case.
var reservedNames = map[string]struct{}{
	"con": {}, "prn": {}, "aux": {}, "nul": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {}, "com5": {},
	"com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {}, "lpt5": {},
	"lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// replacements maps characters windows forbids in a filename onto ASCII
// stand-ins. They are replaced rather than dropped so a title stays readable.
var replacements = map[rune]rune{
	'<': '(', '>': ')', ':': '-', '"': '\'',
	'/': '-', '\\': '-', '|': '-', '?': '_', '*': '_',
}

// SanitizeSegment makes one path segment usable both on windows and as an S3 key.
func SanitizeSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if sub, bad := replacements[r]; bad {
			b.WriteRune(sub)
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = trimRunes(out, maxSegment)

	// windows rejects a trailing dot or space
	out = strings.TrimRight(out, " .")
	if out == "" {
		return ""
	}
	base := out
	if i := strings.IndexByte(out, '.'); i > 0 {
		base = out[:i]
	}
	if _, bad := reservedNames[strings.ToLower(base)]; bad {
		out = "_" + out
	}
	return out
}

func trimRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return strings.TrimSpace(s[:i])
		}
		n++
	}
	return s
}

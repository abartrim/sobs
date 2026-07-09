package main

import (
	"strings"
	"time"

	"github.com/dlclark/regexp2"
)

// userRegex wraps github.com/dlclark/regexp2 — a backtracking, Unicode-aware regex engine that
// matches Python's `re` module far more closely than Go's stdlib RE2 (regexp). It is used ONLY at
// the in-process sites where the Python oracle compiles a (possibly user-supplied) pattern and
// applies it to arbitrary, possibly non-ASCII text:
//
//   - DLP output masking — masking.py `re.sub(pattern, MASK, text, flags=re.DOTALL)` and
//     `re.compile(..., re.DOTALL)` (compileMaskPattern / validateCustomMaskingPattern).
//   - Tag-rule matching — app.py `_match_single_condition` `re.search(match_value, value)` and the
//     tag/condition regex validation on save.
//
// Paths whose pattern is ultimately EXECUTED by ClickHouse (validate-regex, the log/trace/error
// query filters) deliberately stay on stdlib `regexp`: ClickHouse is itself RE2, so validating
// with RE2 correctly rejects patterns the database could not run.
//
// regexp2 is .NET-flavored, not byte-identical to Python `re`, but it closes the gaps that bite:
// Unicode `\d`/`\w`/`\s`/`\b` and lookahead/lookbehind/backreferences. On ASCII input it matches
// identically to the prior RE2 behavior, so the all-ASCII byte-parity corpus is unaffected.
type userRegex struct{ re *regexp2.Regexp }

// userRegexMatchTimeout bounds backtracking. RE2 was linear-time by construction; regexp2 is not,
// so a pathological (user-supplied) pattern could otherwise hang. The curated default masking
// patterns and the ReDoS save-validation make a real timeout vanishingly unlikely; the corpus
// patterns are benign, so this never fires under parity.
const userRegexMatchTimeout = 1 * time.Second

// compileUserRegex compiles a pattern with regexp2. dotAll mirrors Python re.DOTALL (Singleline:
// `.` matches newlines), used for the masking patterns; tag-rule patterns pass dotAll=false to
// mirror the flag-less `re.search`.
func compileUserRegex(pattern string, dotAll bool) (*userRegex, error) {
	opts := regexp2.None
	if dotAll {
		opts |= regexp2.Singleline
	}
	re, err := regexp2.Compile(pattern, opts)
	if err != nil {
		return nil, err
	}
	re.MatchTimeout = userRegexMatchTimeout
	return &userRegex{re: re}, nil
}

// match mirrors Python re.search(pattern, value): true when the pattern matches anywhere in s.
// A timeout / engine error is treated as no-match (Python re.error -> False at the call sites).
func (u *userRegex) match(s string) bool {
	ok, err := u.re.MatchString(s)
	if err != nil {
		return false
	}
	return ok
}

// replaceAll mirrors Python re.sub(pattern, repl, text): replace every match with repl. repl is a
// literal here (no group references), so its `$` (regexp2/.NET substitution syntax) is escaped to
// keep it literal. On a backtracking timeout the input is returned unchanged (best-effort).
func (u *userRegex) replaceAll(s, repl string) string {
	out, err := u.re.Replace(s, escapeRegexReplacement(repl), -1, -1)
	if err != nil {
		return s
	}
	return out
}

// escapeRegexReplacement neutralizes regexp2's `$` substitution metacharacter so the replacement
// is treated literally (".NET replacement uses $$ for a literal $").
func escapeRegexReplacement(repl string) string {
	return strings.ReplaceAll(repl, "$", "$$")
}

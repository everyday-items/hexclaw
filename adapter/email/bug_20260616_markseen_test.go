package email

// BUG-20260616: MarkSeen sent the flag as "(\\Seen)" (the Go literal had four
// backslashes -> two on the wire) instead of RFC3501's "(\Seen)". The server
// rejected it, so messages were never marked read and got reprocessed/replied
// to on every poll. The existing fake client overrode MarkSeen, masking it.

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestBug20260616_MarkSeenFlagFormat(t *testing.T) {
	var sent bytes.Buffer
	c := &imapClient{
		writer: bufio.NewWriter(&sent),
		// First command tag is A0001; canned OK reply unblocks run().
		reader: bufio.NewReader(strings.NewReader("A0001 OK STORE completed\r\n")),
	}

	if err := c.MarkSeen("42"); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	got := sent.String()
	if !strings.Contains(got, `(\Seen)`) {
		t.Fatalf("MarkSeen must send the RFC3501 system flag (\\Seen), wire bytes were: %q", got)
	}
}

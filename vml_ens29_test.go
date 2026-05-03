// Copyright 2016 - 2026 The excelize Authors. All rights reserved. Use of
// this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package excelize

import (
	"archive/zip"
	"bytes"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAddCommentNoMismatchVMLIndex reproduces the ENS-29 scenario and
// asserts the fix.
//
// Shape of the fixture:
//   - Two sheets.
//   - "SheetWithComments" (Sheet1): already owns xl/comments1.xml and
//     references it from its sheet rels. No VML drawing yet.
//   - "SheetWithVML" (Sheet2): has a legacy VML drawing at
//     xl/drawings/vmlDrawing17.vml (e.g. previously used for ActiveX
//     controls) referenced from its sheet rels, but NO comments file.
//
// Before the fix:
//   Calling AddComment("SheetWithVML", ...) used the sheet's vmlID=17 for
//   both the VML and the comments filename, producing xl/comments17.xml.
//   Separately, calling AddComment("SheetWithComments", ...) attempted to
//   reuse commentsN based on vmlID semantics and could collide with the
//   newly-created comments17 — but the more dangerous failure mode is that
//   a sheet with vmlID=1 and no pre-existing comments file would write into
//   SheetWithComments' xl/comments1.xml, superimposing.
//
// After the fix:
//   - AddComment("SheetWithVML", ...) writes into a fresh, unclaimed
//     commentsN.xml file. SheetWithVML's rels get a new comments rel
//     pointing at that file.
//   - AddComment("SheetWithComments", ...) writes into the sheet's
//     pre-existing xl/comments1.xml (not vmlID-derived).
//   - No two sheets end up referencing the same commentsN.xml.
func TestAddCommentNoMismatchVMLIndex(t *testing.T) {
	f := NewFile()

	// Add a second sheet for the VML-only scenario.
	_, err := f.NewSheet("SheetWithVML")
	assert.NoError(t, err)
	assert.NoError(t, f.SetCellValue("SheetWithVML", "A1", "vml-only sheet"))
	assert.NoError(t, f.SetCellValue("Sheet1", "A1", "comments-only sheet"))

	// Rename Sheet1 so the assertion output is clear.
	assert.NoError(t, f.SetSheetName("Sheet1", "SheetWithComments"))

	// --- Inject the ENS-29 collision shape directly into Pkg ---
	//
	// 1. Give SheetWithComments an existing xl/comments1.xml and wire it
	//    via sheet rels.
	sheet1RelsPath := "xl/worksheets/_rels/sheet1.xml.rels"
	sheet2RelsPath := "xl/worksheets/_rels/sheet2.xml.rels"
	comments1Path := "xl/comments1.xml"

	existingCommentsXML := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<comments xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<authors><author>pre-existing</author></authors>` +
		`<commentList><comment ref="B2" authorId="0"><text><t>pre-existing comment</t></text></comment></commentList>` +
		`</comments>`)
	f.Pkg.Store(comments1Path, existingCommentsXML)

	sheet1Rels := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/comments" Target="../comments1.xml"/>` +
		`</Relationships>`)
	f.Pkg.Store(sheet1RelsPath, sheet1Rels)

	// 2. Give SheetWithVML a legacy VML drawing at index 17 (no comments).
	sheet2Rels := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/vmlDrawing" Target="../drawings/vmlDrawing17.vml"/>` +
		`</Relationships>`)
	f.Pkg.Store(sheet2RelsPath, sheet2Rels)
	vmlBody := []byte(`<xml xmlns:v="urn:schemas-microsoft-com:vml"></xml>`)
	f.Pkg.Store("xl/drawings/vmlDrawing17.vml", vmlBody)

	// Clear any cached readers for these sheet rels so excelize re-reads them.
	f.Relationships.Delete(sheet1RelsPath)
	f.Relationships.Delete(sheet2RelsPath)

	// Mark SheetWithVML's worksheet XML as having a LegacyDrawing pointing
	// at rId1 of its rels, which targets vmlDrawing17.vml.
	ws2, err := f.workSheetReader("SheetWithVML")
	assert.NoError(t, err)
	ws2.LegacyDrawing = &xlsxLegacyDrawing{RID: "rId1"}

	// --- Exercise AddComment on both sheets ---
	assert.NoError(t, f.AddComment("SheetWithComments", Comment{
		Cell: "A1", Author: "ens29", Text: "added to pre-existing comments",
		Paragraph: []RichTextRun{{Text: "added to pre-existing comments"}},
	}))
	assert.NoError(t, f.AddComment("SheetWithVML", Comment{
		Cell: "A1", Author: "ens29", Text: "comment on vml-only sheet",
		Paragraph: []RichTextRun{{Text: "comment on vml-only sheet"}},
	}))

	// --- Save to bytes and inspect the archive ---
	var buf bytes.Buffer
	assert.NoError(t, f.Write(&buf))

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	assert.NoError(t, err)

	read := func(name string) string {
		for _, e := range zr.File {
			if e.Name == name {
				rc, err := e.Open()
				if err != nil {
					t.Fatalf("open %s: %v", name, err)
				}
				defer rc.Close()
				b, err := io.ReadAll(rc)
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				return string(b)
			}
		}
		return ""
	}

	list := func() []string {
		var names []string
		for _, e := range zr.File {
			names = append(names, e.Name)
		}
		return names
	}

	// Invariant 1: the pre-existing comment survives in comments1.xml.
	c1 := read(comments1Path)
	assert.NotEmpty(t, c1, "xl/comments1.xml must still exist")
	assert.Contains(t, c1, "pre-existing comment", "existing comment on SheetWithComments must be preserved in comments1.xml")
	assert.Contains(t, c1, "added to pre-existing comments", "new comment on SheetWithComments must also land in comments1.xml")

	// Invariant 2: the comment added to SheetWithVML must NOT be in comments1.xml.
	assert.NotContains(t, c1, "comment on vml-only sheet", "SheetWithVML's comment must not leak into SheetWithComments' comments1.xml")

	// Invariant 3: SheetWithVML's rels now contain a comments relationship,
	// and the file it points at contains the new comment.
	s2Rels := read(sheet2RelsPath)
	re := regexp.MustCompile(`Target="(\.\./comments\d+\.xml)"[^>]*Type="http://schemas\.openxmlformats\.org/officeDocument/2006/relationships/comments"|Type="http://schemas\.openxmlformats\.org/officeDocument/2006/relationships/comments"[^>]*Target="(\.\./comments\d+\.xml)"`)
	matches := re.FindAllStringSubmatch(s2Rels, -1)
	assert.NotEmpty(t, matches, "SheetWithVML must gain a comments relationship: %s", s2Rels)
	var s2CommentsTarget string
	for _, m := range matches {
		if m[1] != "" {
			s2CommentsTarget = m[1]
		} else {
			s2CommentsTarget = m[2]
		}
	}
	assert.NotEqual(t, "../comments1.xml", s2CommentsTarget, "SheetWithVML must NOT share comments1.xml with SheetWithComments")
	s2CommentsFile := "xl/" + strings.TrimPrefix(s2CommentsTarget, "../")
	c2 := read(s2CommentsFile)
	assert.Contains(t, c2, "comment on vml-only sheet", "SheetWithVML's comment must land in its own rel target %s", s2CommentsFile)

	// Invariant 4: no two sheets end up pointing at the same commentsN.xml.
	s1Rels := read(sheet1RelsPath)
	s1CommentsTarget := extractCommentsTarget(s1Rels)
	assert.Equal(t, "../comments1.xml", s1CommentsTarget, "SheetWithComments rels target must still be ../comments1.xml")
	assert.NotEqual(t, s1CommentsTarget, s2CommentsTarget, "the two sheets must not share a comments file (names=%v)", list())

	// Invariant 5: [Content_Types].xml has an Override for the new sheet's
	// comments file path.
	ct := read("[Content_Types].xml")
	idx, _ := parseCommentsIdx(s2CommentsFile)
	assert.Contains(t, ct, "/xl/comments"+strconv.Itoa(idx)+".xml", "[Content_Types].xml must have an Override for %s", s2CommentsFile)
}

// TestResolveCommentsTarget_Helpers spot-checks the helper functions.
func TestResolveCommentsTarget_Helpers(t *testing.T) {
	t.Run("parseCommentsIdx", func(t *testing.T) {
		cases := []struct {
			in   string
			want int
			ok   bool
		}{
			{"xl/comments1.xml", 1, true},
			{"xl/comments42.xml", 42, true},
			{"../comments7.xml", 7, true},
			{"xl/commentsfoo.xml", 0, false},
			{"xl/comments.xml", 0, false},
			{"xl/drawings/vmlDrawing5.vml", 0, false},
			{"", 0, false},
		}
		for _, c := range cases {
			n, ok := parseCommentsIdx(c.in)
			assert.Equal(t, c.want, n, c.in)
			assert.Equal(t, c.ok, ok, c.in)
		}
	})

	t.Run("nextFreeCommentsIdx skips used indices", func(t *testing.T) {
		f := NewFile()
		f.Pkg.Store("xl/comments1.xml", []byte{})
		f.Pkg.Store("xl/comments3.xml", []byte{})
		// gap at 2 and anything >= 4
		got := f.nextFreeCommentsIdx()
		assert.Equal(t, 2, got)

		f.Pkg.Store("xl/comments2.xml", []byte{})
		got = f.nextFreeCommentsIdx()
		assert.Equal(t, 4, got)
	})

	t.Run("commentsIdxClaimedByOtherSheet honors currentSheet exclusion", func(t *testing.T) {
		f := NewFile()
		rels := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/comments" Target="../comments5.xml"/>` +
			`</Relationships>`)
		f.Pkg.Store("xl/worksheets/_rels/sheet3.xml.rels", rels)

		// Another sheet asking if 5 is claimed: yes.
		assert.True(t, f.commentsIdxClaimedByOtherSheet(5, "xl/worksheets/sheet9.xml"))

		// The owner sheet asking if 5 is claimed by someone ELSE: no.
		assert.False(t, f.commentsIdxClaimedByOtherSheet(5, "xl/worksheets/sheet3.xml"))

		// Unused index: no.
		assert.False(t, f.commentsIdxClaimedByOtherSheet(99, "xl/worksheets/sheet9.xml"))
	})
}

// extractCommentsTarget returns the first comments relationship target in a
// sheet rels XML body, or "" if none.
func extractCommentsTarget(rels string) string {
	re := regexp.MustCompile(`<Relationship[^>]*>`)
	for _, m := range re.FindAllString(rels, -1) {
		if !strings.Contains(m, "/relationships/comments") {
			continue
		}
		tgtRe := regexp.MustCompile(`Target="([^"]+)"`)
		if tm := tgtRe.FindStringSubmatch(m); tm != nil {
			return tm[1]
		}
	}
	return ""
}

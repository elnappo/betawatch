package store

import (
	"reflect"
	"testing"
	"time"
)

func TestUncheckedCommonsTitles(t *testing.T) {
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		e := el(1, now)
		e.Tags = map[string]string{
			"wikimedia_commons":        "File:A.jpg",
			"wikimedia_commons:2":      "File:B.jpg",
			"wikimedia_commons:path":   "0.1,0.2|0.3,0.4",
			"wikimedia_commons:2:path": "0.5,0.6",
		}
		must(t, tx.Save(e))

		e = el(1, now)
		e.ID = 2
		e.Tags = map[string]string{
			"wikimedia_commons":   "File:A.jpg", // the same file as element 1
			"wikimedia_commons:3": "Category:Crags",
			"image":               "File:D.jpg", // not a wikimedia_commons key
		}
		must(t, tx.Save(e))

		// The photo of version 1 is superseded by version 2, which has
		// none: only what is attached now may contribute a title.
		e = el(1, now)
		e.ID = 3
		e.Tags = map[string]string{"wikimedia_commons": "File:C.jpg"}
		must(t, tx.Save(e))
		e = el(1, now.Add(time.Minute))
		e.ID = 3
		e.Version = 2
		e.Tags = map[string]string{"climbing": "crag"}
		must(t, tx.Save(e))
	})

	got, err := s.UncheckedCommonsTitles()
	must(t, err)
	want := []string{"File:A.jpg", "File:B.jpg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UncheckedCommonsTitles() = %v, want %v", got, want)
	}

	// A title that already has a row is not new: files are checked once.
	must(t, s.AddCommonsFiles([]CommonsFile{{Title: "File:A.jpg", Status: "missing"}}))
	got, err = s.UncheckedCommonsTitles()
	must(t, err)
	want = []string{"File:B.jpg"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UncheckedCommonsTitles() after checking File:A.jpg = %v, want %v", got, want)
	}
}

func TestAddCommonsFilesAddsOnceAndPrunes(t *testing.T) {
	s := open(t)
	now := time.Now()

	save := func(version int, tags map[string]string) {
		t.Helper()
		e := el(1, now.Add(time.Duration(version)*time.Minute))
		e.Version = version
		e.Tags = tags
		apply(t, s, func(tx *Tx) { must(t, tx.Save(e)) })
	}

	save(1, map[string]string{
		"climbing":            "crag",
		"wikimedia_commons":   "File:A.jpg",
		"wikimedia_commons:2": "File:B.jpg",
	})

	width, height, size := int64(4000), int64(3000), int64(1234567)
	mime := "image/jpeg"
	must(t, s.AddCommonsFiles([]CommonsFile{
		{Title: "File:A.jpg", Width: &width, Height: &height, SizeBytes: &size, MIME: &mime, Status: "ok"},
		{Title: "File:B.jpg", Status: "missing"},
	}))

	var f CommonsFile
	var fetchedAt string
	err := s.db.QueryRow(`
		SELECT title, width, height, size_bytes, mime, status, fetched_at
		FROM commons_files WHERE title = 'File:A.jpg'`).
		Scan(&f.Title, &f.Width, &f.Height, &f.SizeBytes, &f.MIME, &f.Status, &fetchedAt)
	must(t, err)
	if f.Width == nil || *f.Width != width || f.Height == nil || *f.Height != height ||
		f.SizeBytes == nil || *f.SizeBytes != size || f.MIME == nil || *f.MIME != mime {
		t.Errorf("stored File:A.jpg = %+v, want the imageinfo back", f)
	}
	if _, err := time.Parse(time.RFC3339, fetchedAt); err != nil {
		t.Errorf("fetched_at = %q, want RFC 3339: %v", fetchedAt, err)
	}
	if n := count(t, s, `SELECT count(*) FROM commons_files WHERE title = 'File:B.jpg' AND width IS NULL AND height IS NULL AND size_bytes IS NULL AND mime IS NULL`); n != 1 {
		t.Errorf("missing file row = %d, want 1 with NULL imageinfo", n)
	}

	// A file already checked keeps the first answer even if a later call
	// offers different imageinfo: Commons files are treated as immutable.
	otherWidth := int64(1)
	must(t, s.AddCommonsFiles([]CommonsFile{{Title: "File:A.jpg", Width: &otherWidth, Status: "ok"}}))
	if n := count(t, s, `SELECT count(*) FROM commons_files WHERE title = 'File:A.jpg' AND width = 4000`); n != 1 {
		t.Error("File:A.jpg was rewritten, want the first check's answer kept")
	}

	// B is no longer attached: the next pass prunes its row, without
	// looking anything up.
	save(2, map[string]string{"climbing": "crag", "wikimedia_commons": "File:A.jpg"})
	must(t, s.AddCommonsFiles(nil))
	if n := count(t, s, `SELECT count(*) FROM commons_files`); n != 1 {
		t.Errorf("rows after pruning = %d, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM commons_files WHERE title = 'File:A.jpg'`); n != 1 {
		t.Errorf("File:A.jpg rows = %d, want 1 (kept)", n)
	}
}

func TestAddCommonsFilesKeepsTableOnFailure(t *testing.T) {
	s := open(t)
	now := time.Now()
	e := el(1, now)
	e.Tags = map[string]string{"wikimedia_commons": "File:Old.jpg"}
	apply(t, s, func(tx *Tx) { must(t, tx.Save(e)) })
	must(t, s.AddCommonsFiles([]CommonsFile{{Title: "File:Old.jpg", Status: "missing"}}))

	// The second row carries a status the schema does not allow, after the
	// first was inserted: the transaction must roll back everything.
	err := s.AddCommonsFiles([]CommonsFile{
		{Title: "File:New.jpg", Status: "missing"},
		{Title: "File:Broken.jpg", Status: "not a status"},
	})
	if err == nil {
		t.Fatal("AddCommonsFiles with an invalid status succeeded, want an error")
	}
	if n := count(t, s, `SELECT count(*) FROM commons_files WHERE title = 'File:Old.jpg'`); n != 1 {
		t.Error("previous contents lost after a failed check")
	}
	if n := count(t, s, `SELECT count(*) FROM commons_files WHERE title = 'File:New.jpg'`); n != 0 {
		t.Error("partially added row survived a failed check")
	}
}

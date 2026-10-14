package store

import "time"

// CommonsFile is one row of commons_files: one distinct value of a live
// element's wikimedia_commons* tags that names a File:, and what the
// Commons API said about it.
type CommonsFile struct {
	Title     string
	Width     *int64
	Height    *int64
	SizeBytes *int64
	MIME      *string
	Status    string
}

// UncheckedCommonsTitles returns every distinct File: value of the
// wikimedia_commons* tags of live elements that commons_files has no row for
// yet, sorted: the files a check still has to look up. Values of those keys
// that do not start with File: are not files: :path annotations hold
// coordinates, and Category: values name a category. Superseded versions are
// not consulted either: the tags of live elements are what matters.
//
// Files are treated as immutable, so a title that already has a row is never
// new again while the row exists.
func (s *Store) UncheckedCommonsTitles() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT je.value
		FROM elements e, json_each(e.tags) AS je
		WHERE je.key LIKE 'wikimedia_commons%'
		  AND je.value LIKE 'File:%'
		  AND NOT EXISTS (SELECT 1 FROM commons_files c WHERE c.title = je.value)
		ORDER BY je.value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var titles []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		titles = append(titles, t)
	}
	return titles, rows.Err()
}

// AddCommonsFiles inserts a row for each of files and drops rows for titles
// no live element references anymore, in one transaction, so the table ends
// up mirroring the current references. An existing row is left untouched,
// since Commons files are treated as immutable: a title is looked up once,
// not once per run. The prune comes after the inserts, so a title that
// stopped being referenced between its lookup and this write is dropped in
// the same pass.
func (s *Store) AddCommonsFiles(files []CommonsFile) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO commons_files (title, width, height, size_bytes, mime, status, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (title) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	fetchedAt := ts(time.Now())
	for _, f := range files {
		if _, err := stmt.Exec(f.Title, f.Width, f.Height, f.SizeBytes, f.MIME, f.Status, fetchedAt); err != nil {
			return err
		}
	}

	_, err = tx.Exec(`
		DELETE FROM commons_files
		WHERE title NOT IN (
			SELECT DISTINCT je.value
			FROM elements e, json_each(e.tags) AS je
			WHERE je.key LIKE 'wikimedia_commons%' AND je.value LIKE 'File:%'
		)`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

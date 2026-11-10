package store

// The hub's wall of pictures (PLAN.md, Pictures): every picture a post
// carries as its own — an attached one (embeds, a picture by its MIME
// type) or an IPFS link the hub keeps a copy of (pictures, status ok) —
// newest first by the post's own time, the time the Archives count a
// post on. Never a card's picture or a video's poster: those are the
// hub's derivation, not the author's content.

// WallPicture is one picture on the wall with the post it came in.
type WallPicture struct {
	Post, Author, AuthorName string
	At                       int64 // the post's time (postTimeSQL), not when the hub fetched the picture
	CID, MIME, Alt           string
	Width, Height            int // an attached picture's declared size; 0 for a linked one
}

// wallSpill is how many rows past the limit Wall reads so a page can end
// at a post's last picture: a post holds at most four attached pictures
// and four linked ones.
const wallSpill = 8

var wallQuery = `
SELECT p.id, p.author, COALESCE(pr.name, ''), ` + postTimeSQL + `, x.cid, x.mime, x.alt, x.width, x.height
FROM (SELECT post, idx, cid, mime, alt, width, height, 0 AS kind FROM embeds WHERE mime LIKE 'image/%'
      UNION ALL SELECT post, idx, cid, mime, '' AS alt, 0 AS width, 0 AS height, 1 AS kind FROM pictures WHERE status = 'ok' AND cid <> '') x
JOIN posts p ON p.id = x.post LEFT JOIN profiles pr ON pr.id = p.author
WHERE (` + postTimeSQL + `, p.id) < (?, ?)`

// Wall is a page of the wall: the pictures of the posts older than the
// cursor post before ("" for the newest), about limit of them — the page
// ends at a post's last picture, so one post's pictures never split over
// two pages — each post's attached pictures first, then its linked ones,
// each in its own order. author narrows the wall to one profile's posts.
// more says whether older pictures remain, and the last picture's Post
// is then the next page's cursor. An unknown cursor is ErrNotFound.
func (s *Store) Wall(author, before string, limit int) (pics []WallPicture, more bool, err error) {
	at, bid, err := s.dayCursor(before, true)
	if err != nil {
		return nil, false, err
	}
	q, args := wallQuery, []any{at, bid}
	if author != "" {
		q += ` AND p.author = ?`
		args = append(args, author)
	}
	q += ` ORDER BY ` + postTimeSQL + ` DESC, p.id DESC, x.kind, x.idx LIMIT ?`
	rows, err := s.db.Query(q, append(args, limit+wallSpill)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	all := []WallPicture{}
	for rows.Next() {
		var p WallPicture
		if err := rows.Scan(&p.Post, &p.Author, &p.AuthorName, &p.At, &p.CID, &p.MIME, &p.Alt, &p.Width, &p.Height); err != nil {
			return nil, false, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(all) <= limit {
		return all, false, nil
	}
	n := limit
	for n < len(all) && all[n].Post == all[n-1].Post {
		n++
	}
	return all[:n], n < len(all), nil
}

// WallCount is how many pictures the wall holds, one profile's with
// author.
func (s *Store) WallCount(author string) (int, error) {
	q := `SELECT COUNT(*) FROM (SELECT post FROM embeds WHERE mime LIKE 'image/%'
	UNION ALL SELECT post FROM pictures WHERE status = 'ok' AND cid <> '') x JOIN posts p ON p.id = x.post`
	var args []any
	if author != "" {
		q += ` WHERE p.author = ?`
		args = append(args, author)
	}
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

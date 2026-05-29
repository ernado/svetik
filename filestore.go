package lilith

import "io"

// FileStore stores a blob and returns a public URL for it. Used to host media
// (e.g. photos) so it can be passed to the model as an image URL.
type FileStore interface {
	Upload(r io.Reader) (string, error)
}

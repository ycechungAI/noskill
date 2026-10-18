package anime

import "context"

// Downloader is the torrent app the tracker drives. Every implementation only adds torrents with
// noskill's label or category, only lists those for filing, and removes them keeping their files
// (noskill trashes the originals itself after filing). All may read everything, to avoid adding
// a torrent the user already has.
type Downloader interface {
	Name() string
	Ensure(ctx context.Context) error // create the category or check the RPC works
	AddTorrent(ctx context.Context, torrentURL string) error
	Completed(ctx context.Context) ([]Torrent, error) // noskill's finished torrents, oldest first
	Torrent(ctx context.Context, hash string) (Torrent, error)
	Files(ctx context.Context, hash string) ([]TorrentFile, error)
	// Stop stops a finished torrent seeding (the files stay where they are). Only noskill's own.
	Stop(ctx context.Context, hash string) error
	RemoveTorrent(ctx context.Context, hash string) error
	All(ctx context.Context) ([]Torrent, error)
}

var (
	_ Downloader = (*Qbit)(nil)
	_ Downloader = (*Transmission)(nil)
)

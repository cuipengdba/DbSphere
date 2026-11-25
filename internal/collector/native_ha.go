package collector

// NativeHA preserves the engine's position domain and the actual probe sources.
// A missing position or counter stays absent even when the node role is known.
type NativeHA struct {
	Node           string         `json:"node"`
	Role           string         `json:"role"`
	State          string         `json:"state,omitempty"`
	Source         string         `json:"source"`
	Unit           string         `json:"unit"`
	Current        string         `json:"current,omitempty"`
	Received       string         `json:"received,omitempty"`
	Flushed        string         `json:"flushed,omitempty"`
	Replayed       string         `json:"replayed,omitempty"`
	ReplayGap      *int64         `json:"replayGap,omitempty"`
	Peers          []NativeHAPeer `json:"peers,omitempty"`
	PeersObserved  bool           `json:"peersObserved"`
	PeersTruncated bool           `json:"peersTruncated,omitempty"`
	Archive        *NativeArchive `json:"archive,omitempty"`
	WatchGroup     string         `json:"watchGroup,omitempty"`
	WatchState     string         `json:"watchState,omitempty"`
	Switches       *int64         `json:"switches,omitempty"`
}

type NativeHAPeer struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	State       string `json:"state,omitempty"`
	Sync        string `json:"sync,omitempty"`
	Sent        string `json:"sent,omitempty"`
	Received    string `json:"received,omitempty"`
	Flushed     string `json:"flushed,omitempty"`
	Replayed    string `json:"replayed,omitempty"`
	TransferGap *int64 `json:"transferGap,omitempty"`
	ReplayGap   *int64 `json:"replayGap,omitempty"`
	SendCode    *int64 `json:"sendCode,omitempty"`
	ApplySource string `json:"applySource,omitempty"`
}

type NativeArchive struct {
	Archived *int64 `json:"archived,omitempty"`
	Failed   *int64 `json:"failed,omitempty"`
	LastOK   string `json:"lastArchivedAt,omitempty"`
	LastFail string `json:"lastFailedAt,omitempty"`
	ResetAt  string `json:"resetAt,omitempty"`
	State    string `json:"state,omitempty"`
	Error    *bool  `json:"error,omitempty"`
}

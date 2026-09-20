package download

// DownloadResult is the outcome of a successful download + verification.
type DownloadResult struct {
	DestinationPath  string `json:"destination_path"`
	SizeBytes        int64  `json:"size_bytes"`
	ChecksumVerified bool   `json:"checksum_verified"`
}

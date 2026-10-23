// Command file_index_downloader downloads every Canvas course file and optionally uploads them to OpenAI
// Vector Stores.
//
//	file_index_downloader                 full download + optional vector upload
//	file_index_downloader --upload-only   skip downloads, upload existing files
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"canvas_go/internal/downloader"
	"canvas_go/internal/env"
)

func main() {
	env.Load()
	uploadOnly := flag.Bool("upload-only", false, "Upload previously downloaded files to the Vector Store without downloading")
	skipDownload := flag.Bool("skip-download", false, "Alias for --upload-only")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	downloader.Main(ctx, *uploadOnly || *skipDownload)
	if ctx.Err() != nil {
		s := downloader.CurrentStats()
		fmt.Println("\n\n⚠️  Operation interrupted by user")
		if s.FilesDownloaded > 0 {
			fmt.Printf("Downloaded: %d file(s)\n", s.FilesDownloaded)
		}
		if s.FilesUploadedToVectorStore > 0 {
			fmt.Printf("Uploaded: %d file(s)\n", s.FilesUploadedToVectorStore)
		}
	}
}

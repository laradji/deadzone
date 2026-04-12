package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func downloadIfMissing(path, url string) error {
	if _, err := os.Stat(path); err == nil {
		info, _ := os.Stat(path)
		fmt.Printf("  cached: %s (%.1f MB)\n", path, float64(info.Size())/(1024*1024))
		return nil
	}

	fmt.Printf("  downloading %s ...\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("  downloaded: %s (%.1f MB)\n", path, float64(n)/(1024*1024))
	return nil
}

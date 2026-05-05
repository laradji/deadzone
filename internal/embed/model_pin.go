// model_pin.go pins the exact HuggingFace files the hugot embedder
// expects to find on disk in DefaultCacheDir(). It is the source of
// truth the OCI image build (scrape-pack.yml's docker job, via the
// hidden `deadzone hugot-meta` subcommand) reads to stage the model
// weights into the image at build time, mirroring how
// internal/ort.pinnedReleases drives libonnxruntime staging.
//
// Why pinned to a specific revision instead of "main": the hugot
// constructor (hugot.go:128) only stat-checks the ONNX file to decide
// whether to re-download. A silent upstream change to tokenizer.json or
// config.json on `main` would slip past that check and, post-#207's
// bake, would also be invisible to the OCI image refresh story —
// scrape-pack.yml would happily re-curl the same URL and get whatever
// HF served that day. Pinning the commit here makes "the bytes I
// shipped match the bytes the test corpus was scraped against" a
// build-time invariant, the same way internal/ort.Version does for the
// shared library.
//
// Bumping ModelRevision: re-fetch the 6 files at the new commit, run
// `shasum -a 256` on each, paste the digests into pinnedModelFiles. The
// JSON-shape test in cmd/deadzone/hugot_meta_test.go locks the
// 6-file shape so a forgotten file fails CI loud, not at scrape-pack
// runtime when curl 404s.
//
// hugot.go is deliberately NOT modified to consume this table.
// PinnedModel is exposed for OUT-OF-BAND stagers only (the OCI image
// build, air-gapped install scripts). The runtime download path
// continues to delegate to hugot's DownloadModel — that path stays
// alive for native binaries (Brew tap / tarball / AppImage) whose
// cache lives outside any container and survives across MCP-client
// restarts. The image short-circuits because the cache is already
// populated by COPY at build time, not because hugot.go knows about
// the pin.

package embed

import "strings"

// ModelRevision is the HuggingFace commit SHA the pinned files were
// fetched from. It is the model-side equivalent of internal/ort.Version
// — the operator-facing identifier of "which exact bytes are baked in
// this image." Captured from the HuggingFace API:
//
//	curl -fsSL https://huggingface.co/api/models/nomic-ai/nomic-embed-text-v1.5 | jq -r .sha
//
// at the time the bake was authored (2026-05-05). HF commit SHAs are
// 40-hex-char immutable refs — point-in-time snapshots, not branches —
// so this revision will resolve to the same bytes forever as long as
// the model repository remains public.
const ModelRevision = "e9b6763023c676ca8431644204f50c2b100d9aab"

// hfBaseURL is the HuggingFace download endpoint. Split out so a
// future air-gapped mirror can override it via a build-time linker
// flag without forking the constants table. The /resolve/<sha>/<path>
// shape is HF's content-addressable URL — bytes are stable per (repo,
// sha, path) tuple.
const hfBaseURL = "https://huggingface.co/" + DefaultHugotModel + "/resolve/" + ModelRevision

// pinnedModelFile describes one file the hugot embedder needs to find
// in its model cache directory. Every field is required.
type pinnedModelFile struct {
	// SourcePath is the path inside the HF repository at ModelRevision.
	// Used to construct the download URL; the destination filename on
	// disk is DestName.
	SourcePath string
	// SHA256 is the hex digest of the file bytes at the pinned revision.
	// Verified by the staging step before the bytes hit the image build
	// context, so an HF repo deletion or a transparent-proxy cache
	// poisoning event fails the build instead of silently shipping
	// wrong-version weights.
	SHA256 string
	// DestName is the basename the file must have inside the model
	// directory for hugot's NewPipeline to find it. For most files this
	// matches path.Base(SourcePath); the ONNX file is the exception —
	// HF stores it at onnx/model_quantized.onnx, but hugot's
	// DownloadModel flattens it to model_quantized.onnx in the cache,
	// which is the path the FeatureExtractionConfig.OnnxFilename
	// constant (hugot.go:154) points at.
	DestName string
}

// pinnedModelFiles enumerates every file hugot.NewPipeline reads from
// the model directory at NewORTSession time. Empirically derived from
// listing the directory after a successful first-run download:
//
//	ls $DEADZONE_HUGOT_CACHE/nomic-ai_nomic-embed-text-v1.5/
//	# config.json model_quantized.onnx special_tokens_map.json
//	# tokenizer.json tokenizer_config.json vocab.txt
//
// Missing any one of these files makes hugot.NewPipeline panic at
// session-construction time, which under --network none in the OCI
// image becomes a hard MCP-client startup failure with no recovery
// path — the whole point of #207 is that the cache is preloaded so
// hugot never reaches its DownloadModel branch. Adding a file here
// (e.g. a future hugot release that requires a new sentencepiece
// model) means appending one entry; no other code path needs to change.
var pinnedModelFiles = []pinnedModelFile{
	{
		SourcePath: "config.json",
		SHA256:     "9ab00bd92cee80a569f708140b7b6c1661a65891ff3765b1519e181ba2f2c92b",
		DestName:   "config.json",
	},
	{
		SourcePath: "tokenizer.json",
		SHA256:     "d241a60d5e8f04cc1b2b3e9ef7a4921b27bf526d9f6050ab90f9267a1f9e5c66",
		DestName:   "tokenizer.json",
	},
	{
		SourcePath: "tokenizer_config.json",
		SHA256:     "d7e0000bcc80134debd2222220427e6bf5fa20a669f40a0d0d1409cc18e0a9bc",
		DestName:   "tokenizer_config.json",
	},
	{
		SourcePath: "special_tokens_map.json",
		SHA256:     "5d5b662e421ea9fac075174bb0688ee0d9431699900b90662acd44b2a350503a",
		DestName:   "special_tokens_map.json",
	},
	{
		SourcePath: "vocab.txt",
		SHA256:     "07eced375cec144d27c900241f3e339478dec958f92fddbc551f295c992038a3",
		DestName:   "vocab.txt",
	},
	{
		// The ONNX file lives under onnx/ in the HF repo but hugot
		// flattens it to the model dir root on download (hugot.go:134
		// "opts.OnnxFilePath = "onnx/" + onnxFilename" + the downloader
		// copies to modelDir/<basename>). The image staging mirrors that
		// flatten by setting DestName = onnxFilename without the onnx/
		// prefix.
		SourcePath: "onnx/" + onnxFilename,
		SHA256:     "b4342336debaea79de872370664b0aaeb67dea4605513d00ee236ea871a81f27",
		DestName:   onnxFilename,
	},
}

// PinnedModelFile is the public-facing version of pinnedModelFile,
// returned by PinnedModel. Field names are stable JSON contract
// surface — cmd/deadzone/hugot_meta.go marshals these directly and
// scrape-pack.yml's docker job parses the JSON with jq. A breaking
// change here breaks the OCI image build before it can fail loud at
// `docker buildx build` time.
type PinnedModelFile struct {
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	DestName string `json:"dest_name"`
}

// ModelDestDirname is the basename of the directory hugot expects the
// model files to live in, computed from DefaultHugotModel via the same
// "/" → "_" rule hugot's downloader applies (hugot.go:127). Exposed so
// the OCI image build and the hidden hugot-meta subcommand share the
// same derivation rather than each reproducing the rule and drifting on
// a future model swap.
func ModelDestDirname() string {
	return strings.ReplaceAll(DefaultHugotModel, "/", "_")
}

// PinnedModel returns the metadata an out-of-band stager needs to
// populate the hugot cache without invoking the embedder runtime:
//
//   - modelID: the HuggingFace model name (DefaultHugotModel).
//   - revision: the pinned commit SHA (ModelRevision).
//   - destDirname: the directory name hugot expects under the cache
//     root (ModelDestDirname()).
//   - files: one entry per file with the absolute download URL,
//     pinned SHA256, and target filename inside destDirname.
//
// Mirrors the shape of internal/ort.PinnedRelease — same out-of-band
// stager use case, same "constants table is the contract" philosophy.
func PinnedModel() (modelID, revision, destDirname string, files []PinnedModelFile) {
	out := make([]PinnedModelFile, len(pinnedModelFiles))
	for i, f := range pinnedModelFiles {
		out[i] = PinnedModelFile{
			URL:      hfBaseURL + "/" + f.SourcePath,
			SHA256:   f.SHA256,
			DestName: f.DestName,
		}
	}
	return DefaultHugotModel, ModelRevision, ModelDestDirname(), out
}

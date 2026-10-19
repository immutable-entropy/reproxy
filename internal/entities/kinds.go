package entities

import (
	"path"
	"strings"
)

// Kind is the type of content a URL serves, for filtering the Explorer.
type Kind string

const (
	KindHTML       Kind = "html"
	KindJavaScript Kind = "javascript"
	KindCSS        Kind = "css"
	KindJSON       Kind = "json"
	KindXML        Kind = "xml"
	KindImage      Kind = "image"
	KindFont       Kind = "font"
	KindMedia      Kind = "media" // audio and video
	KindFile       Kind = "file"  // downloads: documents, archives, binaries
	KindText       Kind = "text"  // plain text, CSV, Markdown...
	KindOther      Kind = "other" // no response yet, or nothing recognisable
)

// Kinds lists every kind, in the order the UI offers them.
var Kinds = []Kind{
	KindHTML, KindJavaScript, KindCSS, KindJSON, KindXML,
	KindImage, KindFont, KindMedia, KindFile, KindText, KindOther,
}

// KindOf classifies content by its media type, or by the path's file
// extension when the media type says nothing useful (missing, or a generic
// octet-stream). A discovered link has only its path to go on.
func KindOf(mediaType, urlPath string) Kind {
	switch mediaType {
	case "", "application/octet-stream", "binary/octet-stream", "application/unknown":
		if k, ok := extKinds[strings.ToLower(path.Ext(urlPath))]; ok {
			return k
		}
		if mediaType == "" {
			return KindOther
		}
		return KindFile
	}
	mt := mediaType
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		return KindHTML
	case strings.Contains(mt, "javascript") || strings.Contains(mt, "ecmascript"):
		return KindJavaScript
	case mt == "text/css":
		return KindCSS
	case strings.HasPrefix(mt, "image/"):
		return KindImage // before XML, for image/svg+xml
	case mt == "application/json" || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "/json"):
		return KindJSON
	case strings.HasSuffix(mt, "/xml") || strings.HasSuffix(mt, "+xml"):
		return KindXML
	case strings.HasPrefix(mt, "font/") || strings.Contains(mt, "font"):
		return KindFont
	case strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/"):
		return KindMedia
	case strings.HasPrefix(mt, "text/"):
		return KindText
	case strings.HasPrefix(mt, "application/"):
		return KindFile
	}
	return KindOther
}

var extKinds = map[string]Kind{
	".html": KindHTML, ".htm": KindHTML, ".xhtml": KindHTML,
	".js": KindJavaScript, ".mjs": KindJavaScript, ".cjs": KindJavaScript,
	".css":  KindCSS,
	".json": KindJSON, ".map": KindJSON, ".webmanifest": KindJSON,
	".xml": KindXML, ".rss": KindXML, ".atom": KindXML, ".xsd": KindXML, ".wsdl": KindXML,
	".png": KindImage, ".jpg": KindImage, ".jpeg": KindImage, ".gif": KindImage, ".webp": KindImage,
	".svg": KindImage, ".ico": KindImage, ".bmp": KindImage, ".avif": KindImage, ".tif": KindImage, ".tiff": KindImage,
	".woff": KindFont, ".woff2": KindFont, ".ttf": KindFont, ".otf": KindFont, ".eot": KindFont,
	".mp4": KindMedia, ".webm": KindMedia, ".mov": KindMedia, ".mp3": KindMedia, ".ogg": KindMedia,
	".wav": KindMedia, ".m4a": KindMedia, ".flac": KindMedia,
	".pdf": KindFile, ".zip": KindFile, ".gz": KindFile, ".tgz": KindFile, ".tar": KindFile, ".7z": KindFile,
	".rar": KindFile, ".doc": KindFile, ".docx": KindFile, ".xls": KindFile, ".xlsx": KindFile,
	".ppt": KindFile, ".pptx": KindFile, ".exe": KindFile, ".msi": KindFile, ".dmg": KindFile,
	".apk": KindFile, ".jar": KindFile, ".wasm": KindFile, ".bin": KindFile,
	".txt": KindText, ".csv": KindText, ".md": KindText, ".log": KindText,
}

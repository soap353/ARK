package images

import "embed"

//go:embed all:druglabels all:archiver all:indexer all:kolibri
var FS embed.FS

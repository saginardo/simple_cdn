package control

import _ "embed"

// These are the canonical edge deployment resources served by the control plane.
//
//go:embed install-edge.sh
var bootstrapEdgeScript string

//go:embed install-edge.service
var bootstrapEdgeService string

//go:embed install-edge-updater.service
var bootstrapEdgeUpdaterService string

//go:embed install-edge-nginx.service
var bootstrapEdgeNginxService string

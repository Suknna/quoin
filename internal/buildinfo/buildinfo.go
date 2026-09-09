// Package buildinfo contains immutable release identity embedded into binaries.
package buildinfo

// Release is set by the image build's linker flags. Its development default
// keeps local go builds identifiable; it is provenance rather than a
// communication compatibility gate.
var Release = "v0.1.0-dev"

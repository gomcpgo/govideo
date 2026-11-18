// Package kernels holds the hot inner loops of the processing pipeline as
// plain functions over slices (SPEC §9.2). These are the scalar reference
// implementations: they are the default build and the oracle that any later
// SIMD variant (behind `goexperiment.simd && !purego`) must match.
package kernels

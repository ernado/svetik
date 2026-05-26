package lilith

// T returns a pointer to v.
func T[V any](v V) *V {
	return &v
}

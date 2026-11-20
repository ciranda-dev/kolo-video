package portable

// Break shrinks the module memory so that the next call into the module
// faults, as a trap of the translated code would. The returned function gives
// the memory back, so that a test can tell a sticky failure from a module that
// is still broken.
func (e *Encoder) Break() (restore func()) {
	m := e.m
	full := *m.Slice()
	*m.Slice() = full[:8]
	return func() { *m.Slice() = full }
}

// Break shrinks the module memory so that the next call into the module
// faults, as a trap of the translated code would.
func (d *Decoder) Break() { *d.m.Slice() = (*d.m.Slice())[:8] }

// Alive reports whether the decoder holds a module.
func (d *Decoder) Alive() bool { return d.m != nil }

// Reserve grows the decoder's packet buffer to n bytes.
func (d *Decoder) Reserve(n int) error { return d.reserve(n) }

// MemorySize returns the module memory in bytes.
func (e *Encoder) MemorySize() int { return len(*e.m.Slice()) }

// MemorySize returns the module memory in bytes.
func (d *Decoder) MemorySize() int { return len(*d.m.Slice()) }

package admission

const guardStep = 64 << 20

type GuardInput struct {
	Pool, SliceCurrent, MemAvailable, Floor, Min uint64
}

// GuardCeiling returns min(Pool, SliceCurrent+MemAvailable-Floor), not below Min.
// active reports that the ceiling is tighter than the pool.
func GuardCeiling(in GuardInput) (ceiling uint64, active bool) {
	ceiling = satSub(satAdd(in.SliceCurrent, in.MemAvailable), in.Floor)
	if ceiling > in.Pool {
		ceiling = in.Pool
	}
	if ceiling < in.Min {
		ceiling = in.Min
	}
	return ceiling, ceiling < in.Pool
}

// GuardApply reports whether next should replace applied: any decrease,
// or an increase of at least 64 MiB.
func GuardApply(applied, next uint64) bool {
	return next < applied || next-applied >= guardStep
}

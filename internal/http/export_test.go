package http

// HoldIngestSlots occupies every ingest slot, as uploads being processed
// would, until release is called.
func HoldIngestSlots(s *Server) (release func()) {
	n := cap(s.ingestSlots)
	for i := 0; i < n; i++ {
		s.ingestSlots <- struct{}{}
	}
	return func() {
		for i := 0; i < n; i++ {
			<-s.ingestSlots
		}
	}
}

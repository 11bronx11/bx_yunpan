package aisystem

// candidatePool gives relevant selected files a place before filling remaining
// slots by global relevance. It preserves the original ranking of chosen rows:
// diversity must not silently become a new score or leave an unranked tail.
func candidatePool(rows []candidate, limit int, diverse bool) []candidate {
	if len(rows) <= limit {
		return rows
	}
	chosen := make([]bool, len(rows))
	count := 0
	if diverse {
		seen := map[string]bool{}
		for i, c := range rows {
			for _, f := range candidateFiles(c) {
				if !seen[f.FileId] {
					chosen[i] = true
					count++
					for _, alias := range candidateFiles(c) {
						seen[alias.FileId] = true
					}
					break
				}
			}
			if count == limit {
				break
			}
		}
	}
	for i := range rows {
		if count == limit {
			break
		}
		if !chosen[i] {
			chosen[i] = true
			count++
		}
	}
	out := make([]candidate, 0, limit)
	for i, c := range rows {
		if chosen[i] {
			out = append(out, c)
		}
	}
	return out
}

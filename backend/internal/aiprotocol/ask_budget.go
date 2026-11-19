package aiprotocol

const DefaultAnswerMaxTokens = 4096

// SelectedAskMultiplier gives a smaller explicit scope more room for detail.
// Zero is the unselected path and keeps the existing budgets.
func SelectedAskMultiplier(fileCount int) int {
	switch {
	case fileCount == 1:
		return 5
	case fileCount >= 2 && fileCount <= 3:
		return 4
	case fileCount >= 4 && fileCount <= 5:
		return 3
	case fileCount >= 6 && fileCount < 10:
		return 2
	default:
		return 1
	}
}

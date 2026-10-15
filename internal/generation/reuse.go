package generation

// FindReusable returns the first match in the reader's current order. Rollback,
// metadata, date, kernel and current flags are deliberately not equality keys.
func FindReusable(list *BuildList, toplevel string, inputs Object) *Build {
	for _, build := range list.Builds {
		if build.Toplevel == toplevel && sameInputPaths(build.Inputs, inputs) {
			return build
		}
	}
	return nil
}

func sameInputPaths(a, b Object) bool {
	if len(a) != len(b) {
		return false
	}
	for _, member := range a {
		value, present := b.Lookup(member.Name)
		left, leftString := member.Value.(string)
		right, rightString := value.(string)
		if !present || !leftString || !rightString || left != right {
			return false
		}
	}
	return true
}

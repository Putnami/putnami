package estimate

// Size is the size of a slice or a plan.
type Size string

const (
	SizeS Size = "S"
	SizeM Size = "M"
	SizeL Size = "L"
)

// ResultProtocolVersion is the version of the JSON result.
const ResultProtocolVersion = 1

// Result is the estimate of a plan: its size, tokens and minutes, and the
// estimate of each slice in plan order.
type Result struct {
	ProtocolVersion int             `json:"protocolVersion"`
	Plan            string          `json:"plan"`
	Issue           string          `json:"issue"`
	Size            Size            `json:"size"`
	Tokens          Range           `json:"tokens"`
	Minutes         Range           `json:"minutes"`
	Slices          []SliceEstimate `json:"slices"`
}

// SliceEstimate is the estimate of one slice. Files is the count the table
// reads: the slice's files plus the allowance of its endpoints and screens.
type SliceEstimate struct {
	Key     string `json:"key"`
	Files   int    `json:"files"`
	Size    Size   `json:"size"`
	Tokens  Range  `json:"tokens"`
	Minutes Range  `json:"minutes"`
}

// Estimate sizes plan with table. path is the plan's path as the result
// names it. The plan's ranges are the sums of its slices' ranges. A range
// whose high minutes reach no more than the S threshold is S, no more than the
// M threshold M, and L above; a slice or a plan that touches a sensitive zone
// is never S.
func Estimate(plan Plan, table Table, path string) Result {
	result := Result{ProtocolVersion: ResultProtocolVersion, Plan: path, Issue: plan.Issue, Slices: []SliceEstimate{}}
	sensitive := false
	for _, slice := range plan.Slices {
		files := slice.Files + slice.Endpoints*table.EndpointFiles + slice.Screens*table.ScreenFiles
		bucket := table.bucket(files)
		result.Slices = append(result.Slices, SliceEstimate{
			Key:     slice.Key,
			Files:   files,
			Size:    table.size(bucket.Minutes, len(slice.SensitiveZones) > 0),
			Tokens:  bucket.Tokens,
			Minutes: bucket.Minutes,
		})
		result.Tokens = result.Tokens.add(bucket.Tokens)
		result.Minutes = result.Minutes.add(bucket.Minutes)
		sensitive = sensitive || len(slice.SensitiveZones) > 0
	}
	result.Size = table.size(result.Minutes, sensitive)
	return result
}

func (t Table) size(minutes Range, sensitive bool) Size {
	switch {
	case minutes.High <= t.SizeMinutes.S && !sensitive:
		return SizeS
	case minutes.High <= t.SizeMinutes.M:
		return SizeM
	}
	return SizeL
}

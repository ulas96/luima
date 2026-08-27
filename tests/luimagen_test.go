package tests

import "github.com/ulas96/luima/luimagen"

// ExampleGenerate shows the shape of a call, not a runnable one — see docs/luimagen.md §5 for
// why this Example carries no Output: comment.
func ExampleGenerate() {
	err := luimagen.Generate(luimagen.Options{
		Type: "User",
		Fields: []luimagen.Field{
			{Name: "PersonalID", Type: "string", PK: true},
			{Name: "Name", Type: "string"},
		},
	})
	if err != nil {
		panic(err)
	}
}

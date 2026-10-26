package classify

const (
	// Model is the TypeSafe model every request pins.
	Model = "jev-1.14.0"
	// Threshold is the probability both questions must reach for Decide.
	Threshold = 0.7
)

// question is one Noul (yes/no) question as TypeSafe's API takes it.
type question struct {
	Instructions string
	True, False  string
}

// factual asks whether the answer is a short factual reply.
var factual = question{
	Instructions: "Is `answer` a short factual reply, such as a weather report, a definition, a time, an amount, or whether something worked?",
	True:         "The answer states facts the person asked for.",
	False:        "The answer explains how something works, walks through steps, lists files or changes, or weighs options.",
}

// listenable asks whether the answer is fully understood by ear.
var listenable = question{
	Instructions: "Can `answer` be understood fully by hearing it once, without seeing it?",
	True:         "Plain sentences that read aloud naturally.",
	False:        "It relies on seeing exact text, such as identifiers, file paths, commands, numbered steps or a list of items.",
}

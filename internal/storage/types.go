// Package storage defines the fact graph data model and SQLite persistence.
package storage

// IdentKind classifies identifiers in the fact graph.
type IdentKind uint8

const (
	IdentFunc IdentKind = iota
	IdentType
	IdentField
	IdentVar
	IdentConst
	IdentMethod
	IdentInterface
	IdentParam
)

// RefKind classifies how one identifier references another.
type RefKind uint8

const (
	RefUse RefKind = iota
	RefEmbed
	RefImplement
	RefOverride
)

// DispatchKind classifies call edge dispatch mechanism.
type DispatchKind uint8

const (
	DispatchStatic DispatchKind = iota
	DispatchInterface
	DispatchFuncValue
	DispatchDefer
	DispatchGo
	DispatchCodegen // synthesized from codegen patterns
)

// FieldOpKind classifies struct field operations.
type FieldOpKind uint8

const (
	FieldRead FieldOpKind = iota
	FieldWrite
	FieldAddressOf
)

// FlowKind classifies data-flow edge certainty.
type FlowKind uint8

const (
	FlowDefinite FlowKind = iota // static call or codegen-synthesized
	FlowMay                      // interface/func-value dispatch (VTA candidate set)
)

// SlotKind classifies positions in function summaries.
type SlotKind uint8

const (
	SlotParam SlotKind = iota
	SlotReceiver
	SlotReturn
	SlotFieldRead
	SlotFieldWrite
	SlotGlobalRead
	SlotGlobalWrite
	SlotCalleeArg
)

// TypeFactKind classifies type relationships.
type TypeFactKind uint8

const (
	TypeImplements TypeFactKind = iota
	TypeEmbeds
	TypeHasMethod
)

// CodegenKind classifies codegen patterns.
type CodegenKind uint8

const (
	CodegenStateify CodegenKind = iota
	CodegenProtobuf
	CodegenGoGenerate
)

// Identifier represents a named entity in the code.
type Identifier struct {
	ID      uint64
	Name    string
	PkgPath string
	Kind    IdentKind
	File    string
	Line    int
	Col     int
	Doc     string
}

// DefRef represents a definition-reference edge.
type DefRef struct {
	DefID   uint64
	RefID   uint64
	RefKind RefKind
}

// CallEdge represents a call relationship.
type CallEdge struct {
	CallerID uint64
	CalleeID uint64
	File     string
	Line     int
	Dispatch DispatchKind
	ArgMap   []ArgMapping
}

// ArgMapping records how a caller argument maps to a callee parameter.
type ArgMapping struct {
	CallerArgIdx   int
	CalleeParamIdx int
}

// FieldOp represents a read or write to a struct field.
type FieldOp struct {
	StructType string
	FieldName  string
	FieldIdx   int
	OpKind     FieldOpKind
	FuncID     uint64
	File       string
	Line       int
}

// FlagBinding connects a CLI flag to a config struct field.
type FlagBinding struct {
	FlagName   string
	FlagPkg    string
	BoundToID  uint64
	DefaultVal string
	Usage      string
	FuncID     uint64
}

// DataFlowEdge represents a data-flow relationship across functions.
type DataFlowEdge struct {
	FromFunc  uint64
	FromSlot  FlowSlot
	ToFunc    uint64
	ToSlot    FlowSlot
	FlowKind  FlowKind
	Condition string
}

// FlowSlot identifies a position in a function's data-flow summary.
type FlowSlot struct {
	Kind     SlotKind
	Index    int
	TypeID   uint64
	FieldIdx int
	CalleeID uint64
}

// TypeFact records a type relationship.
type TypeFact struct {
	TypeID    uint64
	FactKind  TypeFactKind
	RelatedID uint64
}

// CodegenEdge records a codegen relationship.
type CodegenEdge struct {
	SourceID    uint64
	GeneratedID uint64
	GenKind     CodegenKind
	Pattern     string
}

// FuncSummary records how data flows through a function.
type FuncSummary struct {
	FuncID uint64
	Flows  []SummaryFlow
}

// SummaryFlow records one flow path through a function.
type SummaryFlow struct {
	From      FlowSlot
	To        FlowSlot
	FlowKind  FlowKind
	Condition string
}

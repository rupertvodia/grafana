package controller

import "github.com/grafana/river/ast"

// Declare represents the content of a declare block as AST and as plain string.
type Declare struct {
	Block   *ast.BlockStmt
	Content string
}

// NewDeclare creates a new Declare from its AST and its plain string content.
func NewDeclare(block *ast.BlockStmt, content string) *Declare {
	return &Declare{Block: block, Content: content}
}
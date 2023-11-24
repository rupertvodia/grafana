package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/agent/pkg/flow/internal/dag"
	"github.com/grafana/agent/pkg/flow/internal/worker"
	"github.com/grafana/agent/pkg/flow/logging/level"
	"github.com/grafana/agent/pkg/flow/tracing"
	"github.com/grafana/agent/service"
	"github.com/grafana/dskit/backoff"
	"github.com/grafana/river/ast"
	"github.com/grafana/river/diag"
	"github.com/hashicorp/go-multierror"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The Loader builds and evaluates ComponentNodes from River blocks.
type Loader struct {
	log          log.Logger
	tracer       trace.TracerProvider
	globals      ComponentGlobals
	services     []service.Service
	host         service.Host
	componentReg ComponentRegistry
	workerPool   worker.Pool
	// backoffConfig is used to backoff when an updated component's dependencies cannot be submitted to worker
	// pool for evaluation in EvaluateDependencies, because the queue is full. This is an unlikely scenario, but when
	// it happens we should avoid retrying too often to give other goroutines a chance to progress. Having a backoff
	// also prevents log spamming with errors.
	backoffConfig backoff.Config

	mut               sync.RWMutex
	graph             *dag.Graph
	originalGraph     *dag.Graph
	componentNodes    []*ComponentNode
	serviceNodes      []*ServiceNode
	cache             *valueCache
	blocks            []*ast.BlockStmt // Most recently loaded blocks, used for writing
	cm                *controllerMetrics
	cc                *controllerCollector
	moduleExportIndex int
}

// LoaderOptions holds options for creating a Loader.
type LoaderOptions struct {
	// ComponentGlobals contains data to use when creating components.
	ComponentGlobals ComponentGlobals

	Services          []service.Service // Services to load into the DAG.
	Host              service.Host      // Service host (when running services).
	ComponentRegistry ComponentRegistry // Registry to search for components.
	WorkerPool        worker.Pool       // Worker pool to use for async tasks.
}

// NewLoader creates a new Loader. Components built by the Loader will be built
// with co for their options.
func NewLoader(opts LoaderOptions) *Loader {
	var (
		globals  = opts.ComponentGlobals
		services = opts.Services
		host     = opts.Host
		reg      = opts.ComponentRegistry
	)

	if reg == nil {
		reg = DefaultComponentRegistry{}
	}

	l := &Loader{
		log:          log.With(globals.Logger, "controller_id", globals.ControllerID),
		tracer:       tracing.WrapTracerForLoader(globals.TraceProvider, globals.ControllerID),
		globals:      globals,
		services:     services,
		host:         host,
		componentReg: reg,
		workerPool:   opts.WorkerPool,

		// This is a reasonable default which should work for most cases. If a component is completely stuck, we would
		// retry and log an error every 10 seconds, at most.
		backoffConfig: backoff.Config{
			MinBackoff: 1 * time.Millisecond,
			MaxBackoff: 10 * time.Second,
		},

		graph:         &dag.Graph{},
		originalGraph: &dag.Graph{},
		cache:         newValueCache(),
		cm:            newControllerMetrics(globals.ControllerID),
	}
	l.cc = newControllerCollector(l, globals.ControllerID)

	if globals.Registerer != nil {
		globals.Registerer.MustRegister(l.cc)
		globals.Registerer.MustRegister(l.cm)
	}

	return l
}

// Apply loads a new set of components into the Loader. Apply will drop any
// previously loaded component which is not described in the set of River
// blocks.
//
// Apply will reuse existing components if there is an existing component which
// matches the component ID specified by any of the provided River blocks.
// Reused components will be updated to point at the new River block.
//
// Apply will perform an evaluation of all loaded components before returning.
// The provided parentContext can be used to provide global variables and
// functions to components. A child context will be constructed from the parent
// to expose values of other components.
func (l *Loader) Apply(args map[string]any, componentBlocks []*ast.BlockStmt, configBlocks []*ast.BlockStmt, declareBlocks []*ast.BlockStmt) diag.Diagnostics {
	start := time.Now()
	l.mut.Lock()
	defer l.mut.Unlock()
	l.cm.controllerEvaluation.Set(1)
	defer l.cm.controllerEvaluation.Set(0)

	for key, value := range args {
		l.cache.CacheModuleArgument(key, value)
	}
	l.cache.SyncModuleArgs(args)

	newGraph, diags := l.loadNewGraph(args, componentBlocks, configBlocks, declareBlocks, l.isModule(), false, nil)
	if diags.HasErrors() {
		return diags
	}

	var (
		components   = make([]*ComponentNode, 0, len(componentBlocks))
		componentIDs = make([]ComponentID, 0, len(componentBlocks))
		services     = make([]*ServiceNode, 0, len(l.services))
		declareIDs   = make(map[string]struct{})
		exportIDs    = make(map[string]struct{})
		namespaces   = make(map[string]struct{})
	)

	tracer := l.tracer.Tracer("")
	spanCtx, span := tracer.Start(context.Background(), "GraphEvaluate", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	logger := log.With(l.log, "trace_id", span.SpanContext().TraceID())
	level.Info(logger).Log("msg", "starting complete graph evaluation")
	defer func() {
		span.SetStatus(codes.Ok, "")

		duration := time.Since(start)
		level.Info(logger).Log("msg", "finished complete graph evaluation", "duration", duration)
		l.cm.componentEvaluationTime.Observe(duration.Seconds())
	}()

	l.cache.ClearModuleExports()

	// Evaluate all the components.
	_ = dag.WalkTopological(&newGraph, newGraph.Leaves(), func(n dag.Node) error {
		_, span := tracer.Start(spanCtx, "EvaluateNode", trace.WithSpanKind(trace.SpanKindInternal))
		span.SetAttributes(attribute.String("node_id", n.NodeID()))
		defer span.End()

		start := time.Now()
		defer func() {
			level.Info(logger).Log("msg", "finished node evaluation", "node_id", n.NodeID(), "duration", time.Since(start))
		}()

		var err error

		namespaces[n.NodeID()] = struct{}{}

		switch n := n.(type) {
		case *ComponentNode:
			components = append(components, n)
			componentIDs = append(componentIDs, n.ID())

			if err = l.evaluate(logger, n); err != nil {
				var evalDiags diag.Diagnostics
				if errors.As(err, &evalDiags) {
					diags = append(diags, evalDiags...)
				} else {
					diags.Add(diag.Diagnostic{
						Severity: diag.SeverityLevelError,
						Message:  fmt.Sprintf("Failed to build component: %s", err),
						StartPos: ast.StartPos(n.Block()).Position(),
						EndPos:   ast.EndPos(n.Block()).Position(),
					})
				}
			}

		case *ServiceNode:
			services = append(services, n)

			if err = l.evaluate(logger, n); err != nil {
				var evalDiags diag.Diagnostics
				if errors.As(err, &evalDiags) {
					diags = append(diags, evalDiags...)
				} else {
					diags.Add(diag.Diagnostic{
						Severity: diag.SeverityLevelError,
						Message:  fmt.Sprintf("Failed to evaluate service: %s", err),
						StartPos: ast.StartPos(n.Block()).Position(),
						EndPos:   ast.EndPos(n.Block()).Position(),
					})
				}
			}

		case *DeclareComponentNode:
			declareIDs[n.NodeID()] = struct{}{}
			if err = l.evaluate(logger, n); err != nil {
				var evalDiags diag.Diagnostics
				if errors.As(err, &evalDiags) {
					diags = append(diags, evalDiags...)
				} else {
					diags.Add(diag.Diagnostic{
						Severity: diag.SeverityLevelError,
						Message:  fmt.Sprintf("Failed to evaluate declare component node: %s", err),
						StartPos: ast.StartPos(n.Block()).Position(),
						EndPos:   ast.EndPos(n.Block()).Position(),
					})
				}
			}

		case BlockNode:
			if err = l.evaluate(logger, n); err != nil {
				diags.Add(diag.Diagnostic{
					Severity: diag.SeverityLevelError,
					Message:  fmt.Sprintf("Failed to evaluate node for config block: %s", err),
					StartPos: ast.StartPos(n.Block()).Position(),
					EndPos:   ast.EndPos(n.Block()).Position(),
				})
			}
			if exp, ok := n.(*ExportConfigNode); ok {
				exportIDs[n.NodeID()] = struct{}{}
				l.cache.CacheDeclareExport(exp.NodeID(), exp.Value())
				l.cache.CacheModuleExportValue(exp.Label(), exp.Value())
			}
		}

		// We only use the error for updating the span status; we don't return the
		// error because we want to evaluate as many nodes as we can.
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		return nil
	})

	l.componentNodes = components
	l.serviceNodes = services
	l.graph = &newGraph
	l.cache.SyncIDs(componentIDs)
	l.cache.SyncDeclareIDs(declareIDs)
	l.cache.SyncDeclareExportIDs(exportIDs)
	l.cache.SyncNamespaces(namespaces)
	l.blocks = componentBlocks
	l.cm.componentEvaluationTime.Observe(time.Since(start).Seconds())
	if l.globals.OnExportsChange != nil && l.cache.ExportChangeIndex() != l.moduleExportIndex {
		l.moduleExportIndex = l.cache.ExportChangeIndex()
		l.globals.OnExportsChange(l.cache.CreateModuleExports())
	}
	return diags
}

// Cleanup unregisters any existing metrics and optionally stops the worker pool.
func (l *Loader) Cleanup(stopWorkerPool bool) {
	if stopWorkerPool {
		l.workerPool.Stop()
	}
	if l.globals.Registerer == nil {
		return
	}
	l.globals.Registerer.Unregister(l.cm)
	l.globals.Registerer.Unregister(l.cc)
}

// loadNewGraph creates a new graph from the provided blocks and validates it.
func (l *Loader) loadNewGraph(args map[string]any,
	componentBlocks []*ast.BlockStmt,
	configBlocks []*ast.BlockStmt,
	declareBlocks []*ast.BlockStmt,
	inModule bool,
	inDeclare bool,
	parent *nodeTemplates) (dag.Graph, diag.Diagnostics) {

	var g dag.Graph

	nt := NewNodeTemplates(parent)

	// Split component blocks into blocks for components and services.
	componentBlocks, serviceBlocks := l.splitComponentBlocks(componentBlocks)

	// Fill our graph with service blocks, which must be added before any other
	// block.
	diags := make(diag.Diagnostics, 0)
	// TODO: inDeclare should be inModule once we get rid of the old modules
	if !inDeclare {
		diags = append(diags, l.populateServiceNodes(&g, serviceBlocks)...)
	}

	// Fill our graph with config blocks.
	configBlockDiags := l.populateConfigBlockNodes(args, &g, configBlocks, inModule)
	diags = append(diags, configBlockDiags...)

	// Define the order in which the declare blocks should be populated.
	sortedDeclaredBlocks, sortDeclareBlockDiags := l.SortDeclareBlocks(declareBlocks)
	diags = append(diags, sortDeclareBlockDiags...)

	// Fill our graph with declare blocks.
	declareBlockDiags := l.populateDeclareBlockNodes(&g, sortedDeclaredBlocks, &nt)
	diags = append(diags, declareBlockDiags...)

	// Fill our graph with components.
	componentNodeDiags := l.populateComponentNodes(&g, componentBlocks, &nt)
	diags = append(diags, componentNodeDiags...)

	// Write up the edges of the graph
	wireDiags := l.wireGraphEdges(&g)
	diags = append(diags, wireDiags...)

	// Validate graph to detect cycles
	err := dag.Validate(&g)
	if err != nil {
		diags = append(diags, multierrToDiags(err)...)
		return g, diags
	}

	// Copy the original graph, this is so we can have access to the original graph for things like displaying a UI or
	// debug information.
	l.originalGraph = g.Clone()
	// Perform a transitive reduction of the graph to clean it up.
	dag.Reduce(&g)

	return g, diags
}

func (l *Loader) splitComponentBlocks(blocks []*ast.BlockStmt) (componentBlocks, serviceBlocks []*ast.BlockStmt) {
	componentBlocks = make([]*ast.BlockStmt, 0, len(blocks))
	serviceBlocks = make([]*ast.BlockStmt, 0, len(l.services))

	serviceNames := make(map[string]struct{}, len(l.services))
	for _, svc := range l.services {
		serviceNames[svc.Definition().Name] = struct{}{}
	}

	for _, block := range blocks {
		if _, isService := serviceNames[BlockComponentID(block).String()]; isService {
			serviceBlocks = append(serviceBlocks, block)
		} else {
			componentBlocks = append(componentBlocks, block)
		}
	}

	return componentBlocks, serviceBlocks
}

func (l *Loader) SortDeclareBlocks(declareBlocks []*ast.BlockStmt) ([]*ast.BlockStmt, diag.Diagnostics) {
	var diags diag.Diagnostics

	graph := make(map[string][]string)
	inDegree := make(map[string]int)
	blockByLabel := make(map[string]*ast.BlockStmt)

	for _, block := range declareBlocks {
		label := block.Label
		blockByLabel[label] = block
		if _, exists := graph[label]; !exists {
			graph[label] = []string{}
		}
	}

	for _, block := range declareBlocks {
		label := block.Label
		dependencies := l.findDeclareDependencies(block, blockByLabel)
		for _, dep := range dependencies {
			graph[dep] = append(graph[dep], label)
			inDegree[label]++
		}
	}

	var sortedList []*ast.BlockStmt
	queue := []string{}

	for label := range blockByLabel {
		if inDegree[label] == 0 {
			queue = append(queue, label)
		}
	}

	for len(queue) > 0 {
		label := queue[0]
		queue = queue[1:]
		sortedList = append(sortedList, blockByLabel[label])
		for _, dep := range graph[label] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(sortedList) != len(blockByLabel) {
		unresolvedLabels := []string{}
		for label, count := range inDegree {
			if count > 0 {
				unresolvedLabels = append(unresolvedLabels, label)
			}
		}
		sort.Strings(unresolvedLabels)
		diags.Add(diag.Diagnostic{
			Severity: diag.SeverityLevelError,
			Message:  fmt.Sprintf("Detected a cycle in declare dependencies; cannot sort: %v", unresolvedLabels),
		})
		return nil, diags
	}

	return sortedList, diags
}

func (l *Loader) findDeclareDependencies(declareBlock *ast.BlockStmt, blockByLabel map[string]*ast.BlockStmt) []string {
	var dependencies []string
	for _, stmt := range declareBlock.Body {
		if blockStmt, ok := stmt.(*ast.BlockStmt); ok {
			fullName := strings.Join(blockStmt.Name, ".")
			if fullName == "declare" {
				dependencies = append(dependencies, l.findDeclareDependencies(blockStmt, blockByLabel)...)
			} else if _, exists := blockByLabel[fullName]; exists {
				dependencies = append(dependencies, fullName)
			}
		}
	}
	return dependencies
}

// populateServiceNodes adds service nodes to the graph.
func (l *Loader) populateServiceNodes(g *dag.Graph, serviceBlocks []*ast.BlockStmt) diag.Diagnostics {
	var diags diag.Diagnostics

	// First, build the services.
	for _, svc := range l.services {
		id := svc.Definition().Name

		if g.GetByID(id) != nil {
			diags.Add(diag.Diagnostic{
				Severity: diag.SeverityLevelError,
				Message:  fmt.Sprintf("cannot add service %q; node with same ID already exists", id),
			})

			continue
		}

		var node *ServiceNode

		// Check the graph from the previous call to Load to see we can copy an
		// existing instance of ServiceNode.
		if exist := l.graph.GetByID(id); exist != nil {
			node = exist.(*ServiceNode)
		} else {
			node = NewServiceNode(l.host, svc)
		}

		node.UpdateBlock(nil) // Reset configuration to nil.
		g.Add(node)
	}

	// Now, assign blocks to services.
	for _, block := range serviceBlocks {
		blockID := BlockComponentID(block).String()
		node := g.GetByID(blockID).(*ServiceNode)

		// Blocks assigned to services are reset to nil in the previous loop.
		//
		// If the block is non-nil, it means that there was a duplicate block
		// configuring the same service found in a previous iteration of this loop.
		if node.Block() != nil {
			diags.Add(diag.Diagnostic{
				Severity: diag.SeverityLevelError,
				Message:  fmt.Sprintf("duplicate definition of %q", blockID),
				StartPos: ast.StartPos(block).Position(),
				EndPos:   ast.EndPos(block).Position(),
			})
			continue
		}

		node.UpdateBlock(block)
	}

	return diags
}

// populateConfigBlockNodes adds any config blocks to the graph.
func (l *Loader) populateConfigBlockNodes(args map[string]any, g *dag.Graph, configBlocks []*ast.BlockStmt, inModule bool) diag.Diagnostics {
	var (
		diags   diag.Diagnostics
		nodeMap = NewConfigNodeMap()
	)

	for _, block := range configBlocks {
		node, newConfigNodeDiags := NewConfigNode(block, l.globals)
		diags = append(diags, newConfigNodeDiags...)

		if g.GetByID(node.NodeID()) != nil {
			configBlockStartPos := ast.StartPos(block).Position()
			diags.Add(diag.Diagnostic{
				Severity: diag.SeverityLevelError,
				Message:  fmt.Sprintf("%q block already declared at %s", node.NodeID(), configBlockStartPos),
				StartPos: configBlockStartPos,
				EndPos:   ast.EndPos(block).Position(),
			})

			continue
		}

		nodeMapDiags := nodeMap.Append(node)
		diags = append(diags, nodeMapDiags...)
		if diags.HasErrors() {
			continue
		}

		g.Add(node)
	}

	validateDiags := nodeMap.Validate(inModule, args)
	diags = append(diags, validateDiags...)

	// If a logging config block is not provided, we create an empty node which uses defaults.
	if nodeMap.logging == nil && !inModule {
		c := NewDefaultLoggingConfigNode(l.globals)
		g.Add(c)
	}

	// If a tracing config block is not provided, we create an empty node which uses defaults.
	if nodeMap.tracing == nil && !inModule {
		c := NewDefaulTracingConfigNode(l.globals)
		g.Add(c)
	}

	return diags
}

// populateDeclareBlockNodes creates graph templates which can be used by declareComponentNodes
func (l *Loader) populateDeclareBlockNodes(g *dag.Graph, declareBlocks []*ast.BlockStmt, nt *nodeTemplates) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, block := range declareBlocks {
		categorizedBlocks, err := CategorizeStatements(block.Body)
		if err != nil {
			diags.Add(err.(diag.Diagnostic))
			continue
		}
		// Recursive call to create a graph with the content of the declare block.
		templateGraph, ds := l.loadNewGraph(nil, categorizedBlocks.Components, categorizedBlocks.Configs, categorizedBlocks.DeclareBlocks, true, true, nt)
		if len(ds) > 0 {
			diags = append(diags, ds...)
			continue
		}
		nt.AddTemplate(block.Label, &templateGraph)
	}
	return diags
}

// populateComponentNodes adds any components to the graph.
func (l *Loader) populateComponentNodes(g *dag.Graph, componentBlocks []*ast.BlockStmt, nt *nodeTemplates) diag.Diagnostics {
	var (
		diags    diag.Diagnostics
		blockMap = make(map[string]*ast.BlockStmt, len(componentBlocks))
	)
	graphTemplates, err := nt.RetrieveAvailableTemplates()
	if err != nil {
		diags.Add(diag.Diagnostic{
			Severity: diag.SeverityLevelError,
			Message:  err.Error(),
		})
	}
	for _, block := range componentBlocks {
		id := BlockComponentID(block).String()

		if orig, redefined := blockMap[id]; redefined {
			diags.Add(diag.Diagnostic{
				Severity: diag.SeverityLevelError,
				Message:  fmt.Sprintf("Component %s already declared at %s", id, ast.StartPos(orig).Position()),
				StartPos: block.NamePos.Position(),
				EndPos:   block.NamePos.Add(len(id) - 1).Position(),
			})
			continue
		}
		blockMap[id] = block

		// Check the graph from the previous call to Load to see we can copy an
		// existing instance of ComponentNode.
		if exist := l.graph.GetByID(id); exist != nil {
			if c, ok := exist.(*ComponentNode); ok {
				c.UpdateBlock(block)
				g.Add(c)
				continue
			}
		}

		componentName := block.GetBlockName()
		if block.Label == "" {
			diags.Add(diag.Diagnostic{
				Severity: diag.SeverityLevelError,
				Message:  fmt.Sprintf("Component %q must have a label", componentName),
				StartPos: block.NamePos.Position(),
				EndPos:   block.NamePos.Add(len(componentName) - 1).Position(),
			})
			continue
		}

		if graph, exist := graphTemplates[componentName]; exist {
			clonedGraph := graph.DeepClone(id)
			l.MergeSubgraph(g, clonedGraph)
			declareComponentNode := NewDeclareComponentNode(l.globals, block)
			g.Add(declareComponentNode)
			// Connect the corresponding arguments to the declareComponentNode.
			for _, n := range clonedGraph.Nodes() {
				if n.Namespace() == id {
					switch n := n.(type) {
					case *ArgumentConfigNode:
						g.AddEdge(dag.Edge{From: n, To: declareComponentNode})
					}
				}
			}
		} else {
			registration, exists := l.componentReg.Get(componentName)
			if !exists {
				diags.Add(diag.Diagnostic{
					Severity: diag.SeverityLevelError,
					Message:  fmt.Sprintf("Unrecognized component name %q", componentName),
					StartPos: block.NamePos.Position(),
					EndPos:   block.NamePos.Add(len(componentName) - 1).Position(),
				})
				continue
			}
			g.Add(NewComponentNode(l.globals, registration, block))
		}
	}
	return diags
}

func (l *Loader) MergeSubgraph(new *dag.Graph, subgraph *dag.Graph) {
	for _, node := range subgraph.Nodes() {
		if exists := l.graph.GetByID(node.NodeID()); exists != nil {
			if existingComponent, ok := exists.(*ComponentNode); ok {
				if newComponent, ok := node.(*ComponentNode); ok {
					existingComponent.UpdateBlock(newComponent.Block())
					new.Add(existingComponent)
					continue
				}
			}
		}
		new.Add(node)
	}
	new.MergeEdges(subgraph)
}

// Wire up all the related nodes
func (l *Loader) wireGraphEdges(g *dag.Graph) diag.Diagnostics {
	var diags diag.Diagnostics

	for _, n := range g.Nodes() {

		// Nodes within a namespace are already wired.
		if n.Namespace() != "" {
			continue
		}

		// First, wire up dependencies on services.
		switch n := n.(type) {
		case *ServiceNode: // Service depending on other services.
			for _, depName := range n.Definition().DependsOn {
				dep := g.GetByID(depName)
				if dep == nil {
					diags.Add(diag.Diagnostic{
						Severity: diag.SeverityLevelError,
						Message:  fmt.Sprintf("service %q has invalid reference to service %q", n.NodeID(), depName),
					})
					continue
				}

				g.AddEdge(dag.Edge{From: n, To: dep})
			}
		}

		// Finally, wire component references.
		refs, nodeDiags := ComponentReferences(n, g)
		for _, ref := range refs {
			g.AddEdge(dag.Edge{From: n, To: ref.Target})
		}
		diags = append(diags, nodeDiags...)
	}

	return diags
}

// Components returns the current set of loaded components.
func (l *Loader) Components() []*ComponentNode {
	l.mut.RLock()
	defer l.mut.RUnlock()
	return l.componentNodes
}

// Services returns the current set of service nodes.
func (l *Loader) Services() []*ServiceNode {
	l.mut.RLock()
	defer l.mut.RUnlock()
	return l.serviceNodes
}

// Graph returns a copy of the DAG managed by the Loader.
func (l *Loader) Graph() *dag.Graph {
	l.mut.RLock()
	defer l.mut.RUnlock()
	return l.graph.Clone()
}

// OriginalGraph returns a copy of the graph before Reduce was called. This can be used if you want to show a UI of the
// original graph before the reduce function was called.
func (l *Loader) OriginalGraph() *dag.Graph {
	l.mut.RLock()
	defer l.mut.RUnlock()
	return l.originalGraph.Clone()
}

// EvaluateDependencies sends components which depend directly on components in updatedNodes for evaluation to the
// workerPool. It should be called whenever components update their exports.
// It is beneficial to call EvaluateDependencies with a batch of components, as it will enqueue the entire batch before
// the worker pool starts to evaluate them, resulting in smaller number of total evaluations when
// node updates are frequent. If the worker pool's queue is full, EvaluateDependencies will retry with a backoff until
// it succeeds or until the ctx is cancelled.
func (l *Loader) EvaluateDependencies(ctx context.Context, updatedNodes []*ComponentNode) {
	if len(updatedNodes) == 0 {
		return
	}
	tracer := l.tracer.Tracer("")
	spanCtx, span := tracer.Start(context.Background(), "SubmitDependantsForEvaluation", trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(attribute.Int("originators_count", len(updatedNodes)))
	span.SetStatus(codes.Ok, "dependencies submitted for evaluation")
	defer span.End()

	l.cm.controllerEvaluation.Set(1)
	defer l.cm.controllerEvaluation.Set(0)

	l.mut.RLock()
	defer l.mut.RUnlock()

	dependenciesToParentsMap := make(map[dag.Node]*ComponentNode)
	for _, parent := range updatedNodes {
		// Make sure we're in-sync with the current exports of parent.
		l.cache.CacheExports(parent.ID(), parent.Exports())
		// We collect all nodes directly incoming to parent.
		_ = dag.WalkIncomingNodes(l.graph, parent, func(n dag.Node) error {
			dependenciesToParentsMap[n] = parent
			return nil
		})
	}

	// Submit all dependencies for asynchronous evaluation.
	// During evaluation, if a node's exports change, Flow will add it to updated nodes queue (controller.Queue) and
	// the Flow controller will call EvaluateDependencies on it again. This results in a concurrent breadth-first
	// traversal of the nodes that need to be evaluated.
	for n, parent := range dependenciesToParentsMap {
		dependantCtx, span := tracer.Start(spanCtx, "SubmitForEvaluation", trace.WithSpanKind(trace.SpanKindInternal))
		span.SetAttributes(attribute.String("node_id", n.NodeID()))
		span.SetAttributes(attribute.String("originator_id", parent.NodeID()))

		// Submit for asynchronous evaluation with retries and backoff. Don't use range variables in the closure.
		var (
			nodeRef, parentRef = n, parent
			retryBackoff       = backoff.New(ctx, l.backoffConfig)
			err                error
		)
		for retryBackoff.Ongoing() {
			err = l.workerPool.SubmitWithKey(nodeRef.NodeID(), func() {
				l.concurrentEvalFn(nodeRef, dependantCtx, tracer, parentRef)
			})
			if err != nil {
				level.Error(l.log).Log(
					"msg", "failed to submit node for evaluation - the agent is likely overloaded "+
						"and cannot keep up with evaluating components - will retry",
					"err", err,
					"node_id", n.NodeID(),
					"originator_id", parent.NodeID(),
					"retries", retryBackoff.NumRetries(),
				)
				retryBackoff.Wait()
			} else {
				break
			}
		}
		span.SetAttributes(attribute.Int("retries", retryBackoff.NumRetries()))
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "node submitted for evaluation")
		}
		span.End()
	}

	// Report queue size metric.
	l.cm.evaluationQueueSize.Set(float64(l.workerPool.QueueSize()))
}

// concurrentEvalFn returns a function that evaluates a node and updates the cache. This function can be submitted to
// a worker pool for asynchronous evaluation.
func (l *Loader) concurrentEvalFn(n dag.Node, spanCtx context.Context, tracer trace.Tracer, parent *ComponentNode) {
	start := time.Now()
	l.cm.dependenciesWaitTime.Observe(time.Since(parent.lastUpdateTime.Load()).Seconds())
	_, span := tracer.Start(spanCtx, "EvaluateNode", trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(attribute.String("node_id", n.NodeID()))
	defer span.End()

	defer func() {
		duration := time.Since(start)
		level.Info(l.log).Log("msg", "finished node evaluation", "node_id", n.NodeID(), "duration", duration)
		l.cm.componentEvaluationTime.Observe(duration.Seconds())
	}()

	var err error
	switch n := n.(type) {
	case BlockNode:
		ectx := l.cache.BuildContext(n)
		evalErr := n.Evaluate(ectx)

		// Only obtain loader lock after we have evaluated the node, allowing for concurrent evaluation.
		l.mut.RLock()
		err = l.postEvaluate(l.log, n, evalErr)

		// Additional post-evaluation steps necessary for module exports.
		if exp, ok := n.(*ExportConfigNode); ok {
			l.cache.CacheDeclareExport(exp.NodeID(), exp.Value())
			l.cache.CacheModuleExportValue(exp.Label(), exp.Value())
		}
		if l.globals.OnExportsChange != nil && l.cache.ExportChangeIndex() != l.moduleExportIndex {
			// Upgrade to write lock to update the module exports.
			l.mut.RUnlock()
			l.mut.Lock()
			defer l.mut.Unlock()
			// Check if the update still needed after obtaining the write lock and perform it.
			if l.cache.ExportChangeIndex() != l.moduleExportIndex {
				l.globals.OnExportsChange(l.cache.CreateModuleExports())
				l.moduleExportIndex = l.cache.ExportChangeIndex()
			}
		} else {
			// No need to upgrade to write lock, just release the read lock.
			l.mut.RUnlock()
		}
	}

	// We only use the error for updating the span status
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "node successfully evaluated")
	}
}

// evaluate constructs the final context for the BlockNode and
// evaluates it. mut must be held when calling evaluate.
func (l *Loader) evaluate(logger log.Logger, bn BlockNode) error {
	ectx := l.cache.BuildContext(bn)
	err := bn.Evaluate(ectx)
	return l.postEvaluate(logger, bn, err)
}

// postEvaluate is called after a node has been evaluated. It updates the caches and logs any errors.
// mut must be held when calling postEvaluate.
func (l *Loader) postEvaluate(logger log.Logger, bn BlockNode, err error) error {
	l.cache.CacheNamespace(bn.NodeID(), bn.Namespace())
	switch c := bn.(type) {
	case *DeclareComponentNode:
		err = l.cache.CacheDeclare(c.NodeID(), c.Arguments())
	case *ComponentNode:
		// Always update the cache both the arguments and exports, since both might
		// change when a component gets re-evaluated. We also want to cache the arguments and exports in case of an error
		l.cache.CacheArguments(c.ID(), c.Arguments())
		l.cache.CacheExports(c.ID(), c.Exports())
	case *ArgumentConfigNode:
		// This part is for new modules
		if namespaceArgs, found := l.cache.declareValues[c.Namespace()]; found {
			parts := strings.Split(c.NodeID(), ".")
			label := parts[len(parts)-1]
			if _, alreadyDeclared := namespaceArgs[label]; !alreadyDeclared {
				if c.Optional() {
					namespaceArgs[label] = c.Default()
				} else {
					// Should be an error here once the old modules are removed
				}
			}
		}

		// This part is for old modules
		if _, found := l.cache.moduleArguments[c.Label()]; !found {
			if c.Optional() {
				l.cache.CacheModuleArgument(c.Label(), c.Default())
			} else {
				// Removing this error because it would be triggered for new modules
				//err = fmt.Errorf("missing required argument %q to module", c.Label())
			}
		}
	}

	if err != nil {
		level.Error(logger).Log("msg", "failed to evaluate config", "node", bn.NodeID(), "err", err)
		return err
	}
	return nil
}

func multierrToDiags(errors error) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, err := range errors.(*multierror.Error).Errors {
		// TODO(rfratto): should this include position information?
		diags.Add(diag.Diagnostic{
			Severity: diag.SeverityLevelError,
			Message:  err.Error(),
		})
	}
	return diags
}

// If the definition of a module ever changes, update this.
func (l *Loader) isModule() bool {
	// Either 1 of these checks is technically sufficient but let's be extra careful.
	return l.globals.OnExportsChange != nil && l.globals.ControllerID != ""
}

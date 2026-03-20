package api

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexagon"
	hxgraph "github.com/hexagon-codes/hexagon/orchestration/graph"
	"github.com/hexagon-codes/hexclaw/adapter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

const (
	nodeStatusPending   = "pending"
	nodeStatusRunning   = "running"
	nodeStatusCompleted = "completed"
	nodeStatusSkipped   = "skipped"
	nodeStatusFailed    = "failed"

	stateKeyInput          = "__workflow_input"
	stateKeyNodeOutputs    = "__workflow_node_outputs"
	stateKeyNodeHandoffs   = "__workflow_node_handoffs"
	stateKeyWorkflowOutput = "__workflow_output"
)

type RunWorkflowRequest struct {
	Input      string            `json:"input"`
	UserID     string            `json:"user_id,omitempty"`
	Platform   string            `json:"platform,omitempty"`
	InstanceID string            `json:"instance_id,omitempty"`
	ChatID     string            `json:"chat_id,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type WorkflowNodeRun struct {
	NodeID       string    `json:"node_id"`
	Type         string    `json:"type"`
	Label        string    `json:"label,omitempty"`
	Status       string    `json:"status"`
	Output       string    `json:"output,omitempty"`
	Error        string    `json:"error,omitempty"`
	AgentRole    string    `json:"agent_role,omitempty"`
	HandoffAgent string    `json:"handoff_agent,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
}

type workflowNode struct {
	ID    string
	Type  string
	Label string
	Data  map[string]any
}

type workflowEdge struct {
	Source string
	Target string
}

type workflowStage struct {
	ID      string
	NodeIDs []string
}

type workflowExecutor struct {
	server *Server
	wf     *WorkflowData
	req    RunWorkflowRequest

	nodes    map[string]*workflowNode
	edges    []workflowEdge
	incoming map[string][]string
	outgoing map[string][]string
	order    map[string]int
	stages   []workflowStage
	sinks    []string

	mu       sync.Mutex
	nodeRuns map[string]*WorkflowNodeRun
}

func newWorkflowExecutor(s *Server, wf *WorkflowData, req RunWorkflowRequest) *workflowExecutor {
	return &workflowExecutor{
		server:   s,
		wf:       wf,
		req:      req,
		nodes:    make(map[string]*workflowNode),
		incoming: make(map[string][]string),
		outgoing: make(map[string][]string),
		order:    make(map[string]int),
		nodeRuns: make(map[string]*WorkflowNodeRun),
	}
}

func (e *workflowExecutor) execute(ctx context.Context, run *WorkflowRun) *WorkflowRun {
	if err := e.parse(); err != nil {
		return e.failedRun(run, err)
	}
	if err := e.buildStages(); err != nil {
		return e.failedRun(run, err)
	}

	if len(e.nodes) == 0 {
		finished := *run
		finished.Status = "completed"
		finished.FinishedAt = time.Now()
		return &finished
	}

	g, err := e.buildGraph()
	if err != nil {
		return e.failedRun(run, err)
	}

	state := hxgraph.MapState{
		stateKeyInput:          e.req.Input,
		stateKeyNodeOutputs:    map[string]string{},
		stateKeyNodeHandoffs:   map[string]string{},
		stateKeyWorkflowOutput: "",
	}

	result, err := g.Run(ctx, state)
	if err != nil {
		return e.failedRun(run, err)
	}

	output := firstNonEmpty(stringStateValue(result, stateKeyWorkflowOutput), e.collectFinalOutput(result))
	finished := *run
	finished.Status = "completed"
	finished.Output = output
	finished.NodeResults = e.listNodeRuns()
	finished.FinishedAt = time.Now()
	return &finished
}

func (e *workflowExecutor) failedRun(run *WorkflowRun, err error) *WorkflowRun {
	finished := *run
	finished.Status = "failed"
	if err != nil {
		finished.Error = err.Error()
	}
	finished.NodeResults = e.listNodeRuns()
	finished.FinishedAt = time.Now()
	return &finished
}

func (e *workflowExecutor) parse() error {
	for i, raw := range e.wf.Nodes {
		nodeMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := stringValue(nodeMap["id"])
		if id == "" {
			return fmt.Errorf("workflow node[%d] 缺少 id", i)
		}
		if _, exists := e.nodes[id]; exists {
			return fmt.Errorf("workflow node id 重复: %s", id)
		}
		data, _ := nodeMap["data"].(map[string]any)
		nodeType := strings.ToLower(stringValue(nodeMap["type"]))
		if nodeType == "" {
			nodeType = "noop"
		}
		label := firstNonEmpty(stringValue(nodeMap["label"]), id)
		e.nodes[id] = &workflowNode{
			ID:    id,
			Type:  nodeType,
			Label: label,
			Data:  cloneMap(data),
		}
		e.order[id] = i
	}

	for _, raw := range e.wf.Edges {
		edgeMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		source := stringValue(edgeMap["source"])
		target := stringValue(edgeMap["target"])
		if source == "" || target == "" {
			continue
		}
		if e.nodes[source] == nil || e.nodes[target] == nil {
			return fmt.Errorf("workflow edge 引用了不存在的节点: %s -> %s", source, target)
		}
		e.edges = append(e.edges, workflowEdge{Source: source, Target: target})
		e.outgoing[source] = append(e.outgoing[source], target)
		e.incoming[target] = append(e.incoming[target], source)
	}

	for id := range e.nodes {
		if len(e.outgoing[id]) == 0 {
			e.sinks = append(e.sinks, id)
		}
	}
	sort.Slice(e.sinks, func(i, j int) bool {
		return e.order[e.sinks[i]] < e.order[e.sinks[j]]
	})
	return nil
}

func (e *workflowExecutor) buildStages() error {
	indegree := make(map[string]int, len(e.nodes))
	for id := range e.nodes {
		indegree[id] = len(e.incoming[id])
	}

	var current []string
	for id, deg := range indegree {
		if deg == 0 {
			current = append(current, id)
		}
	}
	sort.Slice(current, func(i, j int) bool {
		return e.order[current[i]] < e.order[current[j]]
	})

	processed := 0
	stageIndex := 0
	for len(current) > 0 {
		stage := workflowStage{
			ID:      fmt.Sprintf("stage_%02d", stageIndex),
			NodeIDs: append([]string(nil), current...),
		}
		e.stages = append(e.stages, stage)

		nextSet := make(map[string]struct{})
		for _, id := range current {
			processed++
			for _, target := range e.outgoing[id] {
				indegree[target]--
				if indegree[target] == 0 {
					nextSet[target] = struct{}{}
				}
			}
		}

		next := make([]string, 0, len(nextSet))
		for id := range nextSet {
			next = append(next, id)
		}
		sort.Slice(next, func(i, j int) bool {
			return e.order[next[i]] < e.order[next[j]]
		})
		current = next
		stageIndex++
	}

	if processed != len(e.nodes) {
		return fmt.Errorf("workflow DAG 存在环或不可达依赖")
	}
	return nil
}

func (e *workflowExecutor) buildGraph() (*hxgraph.Graph[hxgraph.MapState], error) {
	builder := hexagon.NewGraph[hxgraph.MapState](e.wf.Name)

	for _, stage := range e.stages {
		if len(stage.NodeIDs) == 1 {
			nodeID := stage.NodeIDs[0]
			builder.AddNode(stage.ID, func(ctx context.Context, state hxgraph.MapState) (hxgraph.MapState, error) {
				return e.executeNode(ctx, state, e.nodes[nodeID])
			})
			continue
		}

		handlers := make([]hxgraph.NodeHandler[hxgraph.MapState], 0, len(stage.NodeIDs))
		for _, nodeID := range stage.NodeIDs {
			id := nodeID
			handlers = append(handlers, func(ctx context.Context, state hxgraph.MapState) (hxgraph.MapState, error) {
				return e.executeNode(ctx, state, e.nodes[id])
			})
		}

		builder.AddNodeWithBuilder(hxgraph.ParallelNodeWithMerger(
			stage.ID,
			func(original hxgraph.MapState, outputs []hxgraph.MapState) hxgraph.MapState {
				return mergeWorkflowStates(original, outputs)
			},
			handlers...,
		))
	}

	builder.AddEdge(hexagon.START, e.stages[0].ID)
	for i := 0; i < len(e.stages)-1; i++ {
		builder.AddEdge(e.stages[i].ID, e.stages[i+1].ID)
	}
	builder.AddEdge(e.stages[len(e.stages)-1].ID, hexagon.END)
	return builder.Build()
}

func (e *workflowExecutor) executeNode(ctx context.Context, state hxgraph.MapState, node *workflowNode) (hxgraph.MapState, error) {
	if node == nil {
		return state, nil
	}

	e.markNodeStart(node)
	inputText := e.resolveNodeInput(state, node)

	var (
		output       string
		agentRole    string
		handoffAgent string
		err          error
	)

	switch node.Type {
	case "input":
		output = firstNonEmpty(renderTemplate(firstNonEmpty(stringValue(node.Data["prompt"]), stringValue(node.Data["value"])), state), stringStateValue(state, stateKeyInput), node.Label)

	case "agent":
		selected := e.selectedHandoffForNode(state, node)
		agentRole = firstNonEmpty(stringValue(node.Data["role"]), stringValue(node.Data["agent"]), selected)
		if selected != "" && agentRole != "" && agentRole != selected {
			e.markNodeSkipped(node, agentRole)
			return state, nil
		}
		output, err = e.executeAgent(ctx, node, inputText, agentRole)

	case "handoff", "agent_handoff":
		handoffAgent, err = e.selectHandoffAgent(ctx, node, inputText)
		output = inputText

	case "tool":
		output, err = e.executeTool(ctx, node, inputText, state)

	case "output":
		output = inputText
		state = setStringStateValue(state, stateKeyWorkflowOutput, output)

	default:
		output = inputText
	}

	if err != nil {
		e.markNodeFailed(node, agentRole, handoffAgent, err)
		return state, err
	}

	state = putStringMapStateValue(state, stateKeyNodeOutputs, node.ID, output)
	if handoffAgent != "" {
		state = putStringMapStateValue(state, stateKeyNodeHandoffs, node.ID, handoffAgent)
	}
	e.markNodeCompleted(node, output, agentRole, handoffAgent)
	return state, nil
}

func (e *workflowExecutor) executeAgent(ctx context.Context, node *workflowNode, inputText, role string) (string, error) {
	if e.server.engine == nil {
		return "", fmt.Errorf("engine 未初始化")
	}

	metadata := make(map[string]string, len(e.req.Metadata)+4)
	for k, v := range e.req.Metadata {
		metadata[k] = v
	}
	if role != "" {
		metadata["role"] = role
	}
	if provider := stringValue(node.Data["provider"]); provider != "" {
		metadata["provider"] = provider
	}
	metadata["workflow_id"] = e.wf.ID
	metadata["workflow_node_id"] = node.ID

	reply, err := e.server.engine.Process(ctx, (&agentrouterMessageAdapter{
		UserID:     firstNonEmpty(e.req.UserID, "workflow-"+e.wf.ID),
		Platform:   e.req.Platform,
		InstanceID: e.req.InstanceID,
		ChatID:     e.req.ChatID,
		Content:    inputText,
		Metadata:   metadata,
	}).Message())
	if err != nil {
		return "", err
	}
	if reply == nil {
		return "", nil
	}
	return reply.Content, nil
}

func (e *workflowExecutor) executeTool(ctx context.Context, node *workflowNode, inputText string, state hxgraph.MapState) (string, error) {
	if e.server.mcpMgr == nil {
		return "", fmt.Errorf("mcp manager 未初始化")
	}

	toolName := firstNonEmpty(stringValue(node.Data["tool"]), stringValue(node.Data["name"]))
	if toolName == "" {
		return "", fmt.Errorf("tool 节点缺少 tool 名称")
	}

	args, _ := node.Data["args"].(map[string]any)
	rendered := renderValue(cloneMap(args), state)
	argMap, _ := rendered.(map[string]any)
	if len(argMap) == 0 && inputText != "" {
		argMap = map[string]any{"input": inputText}
	}

	return e.server.mcpMgr.CallTool(ctx, toolName, argMap)
}

func (e *workflowExecutor) selectHandoffAgent(ctx context.Context, node *workflowNode, inputText string) (string, error) {
	explicit := firstNonEmpty(stringValue(node.Data["to_agent"]), stringValue(node.Data["agent"]), stringValue(node.Data["role"]))
	if explicit != "" {
		return explicit, nil
	}

	candidates := stringSlice(node.Data["candidates"])
	if len(candidates) == 0 {
		candidates = e.successorAgentRoles(node.ID)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("handoff 节点 %s 缺少候选 agent", node.ID)
	}

	if e.server.agentRouter != nil {
		req := agentrouter.RouteRequest{
			Platform:   e.req.Platform,
			InstanceID: e.req.InstanceID,
			UserID:     e.req.UserID,
			ChatID:     e.req.ChatID,
		}
		result, _ := e.server.agentRouter.RouteWithFallback(ctx, req, inputText)
		if result != nil && slices.Contains(candidates, result.AgentName) {
			return result.AgentName, nil
		}
	}

	return candidates[0], nil
}

func (e *workflowExecutor) successorAgentRoles(nodeID string) []string {
	var roles []string
	for _, target := range e.outgoing[nodeID] {
		role := firstNonEmpty(stringValue(e.nodes[target].Data["role"]), stringValue(e.nodes[target].Data["agent"]))
		if role != "" && !slices.Contains(roles, role) {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}

func (e *workflowExecutor) resolveNodeInput(state hxgraph.MapState, node *workflowNode) string {
	upstream := e.upstreamOutputs(state, node.ID)
	rendered := renderTemplate(firstNonEmpty(stringValue(node.Data["prompt"]), stringValue(node.Data["input"])), state)
	switch {
	case rendered != "" && upstream != "":
		return rendered + "\n\n[上游结果]\n" + upstream
	case rendered != "":
		return rendered
	case upstream != "":
		return upstream
	case stringStateValue(state, stateKeyInput) != "":
		return stringStateValue(state, stateKeyInput)
	default:
		return node.Label
	}
}

func (e *workflowExecutor) upstreamOutputs(state hxgraph.MapState, nodeID string) string {
	outputs := stringMapStateValue(state, stateKeyNodeOutputs)
	preds := append([]string(nil), e.incoming[nodeID]...)
	sort.Slice(preds, func(i, j int) bool {
		return e.order[preds[i]] < e.order[preds[j]]
	})

	parts := make([]string, 0, len(preds))
	for _, pred := range preds {
		if out := outputs[pred]; out != "" {
			parts = append(parts, out)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (e *workflowExecutor) selectedHandoffForNode(state hxgraph.MapState, node *workflowNode) string {
	handoffs := stringMapStateValue(state, stateKeyNodeHandoffs)
	preds := append([]string(nil), e.incoming[node.ID]...)
	sort.Slice(preds, func(i, j int) bool {
		return e.order[preds[i]] < e.order[preds[j]]
	})
	for _, pred := range preds {
		if e.nodes[pred] != nil && (e.nodes[pred].Type == "handoff" || e.nodes[pred].Type == "agent_handoff") {
			if explicit := firstNonEmpty(
				stringValue(e.nodes[pred].Data["to_agent"]),
				stringValue(e.nodes[pred].Data["agent"]),
				stringValue(e.nodes[pred].Data["role"]),
			); explicit != "" {
				return explicit
			}
			if selected := handoffs[pred]; selected != "" {
				return selected
			}
		}
	}
	if len(handoffs) == 1 {
		for _, selected := range handoffs {
			return selected
		}
	}
	return ""
}

func (e *workflowExecutor) collectFinalOutput(state hxgraph.MapState) string {
	outputs := stringMapStateValue(state, stateKeyNodeOutputs)
	var selected []string
	for _, sink := range e.sinks {
		if out := outputs[sink]; out != "" {
			selected = append(selected, out)
		}
	}
	return strings.Join(selected, "\n\n")
}

func (e *workflowExecutor) markNodeStart(node *workflowNode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	run := e.ensureNodeRun(node)
	run.Status = nodeStatusRunning
	run.StartedAt = time.Now()
}

func (e *workflowExecutor) markNodeCompleted(node *workflowNode, output, role, handoffAgent string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	run := e.ensureNodeRun(node)
	run.Status = nodeStatusCompleted
	run.Output = output
	run.AgentRole = role
	run.HandoffAgent = handoffAgent
	run.FinishedAt = time.Now()
}

func (e *workflowExecutor) markNodeSkipped(node *workflowNode, role string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	run := e.ensureNodeRun(node)
	run.Status = nodeStatusSkipped
	run.AgentRole = role
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	run.FinishedAt = time.Now()
}

func (e *workflowExecutor) markNodeFailed(node *workflowNode, role, handoffAgent string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	run := e.ensureNodeRun(node)
	run.Status = nodeStatusFailed
	run.AgentRole = role
	run.HandoffAgent = handoffAgent
	run.Error = err.Error()
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	run.FinishedAt = time.Now()
}

func (e *workflowExecutor) ensureNodeRun(node *workflowNode) *WorkflowNodeRun {
	if run, ok := e.nodeRuns[node.ID]; ok {
		return run
	}
	run := &WorkflowNodeRun{
		NodeID: node.ID,
		Type:   node.Type,
		Label:  node.Label,
		Status: nodeStatusPending,
	}
	e.nodeRuns[node.ID] = run
	return run
}

func (e *workflowExecutor) listNodeRuns() []WorkflowNodeRun {
	e.mu.Lock()
	defer e.mu.Unlock()

	list := make([]WorkflowNodeRun, 0, len(e.nodes))
	for id, node := range e.nodes {
		run := e.nodeRuns[id]
		if run == nil {
			run = &WorkflowNodeRun{
				NodeID: id,
				Type:   node.Type,
				Label:  node.Label,
				Status: nodeStatusPending,
			}
		}
		list = append(list, *run)
	}
	sort.Slice(list, func(i, j int) bool {
		return e.order[list[i].NodeID] < e.order[list[j].NodeID]
	})
	return list
}

func mergeWorkflowStates(original hxgraph.MapState, outputs []hxgraph.MapState) hxgraph.MapState {
	merged := original.Clone().(hxgraph.MapState)
	combinedOutputs := stringMapStateValue(merged, stateKeyNodeOutputs)
	combinedHandoffs := stringMapStateValue(merged, stateKeyNodeHandoffs)
	var workflowOutputs []string

	for _, out := range outputs {
		for k, v := range stringMapStateValue(out, stateKeyNodeOutputs) {
			combinedOutputs[k] = v
		}
		for k, v := range stringMapStateValue(out, stateKeyNodeHandoffs) {
			combinedHandoffs[k] = v
		}
		if v := stringStateValue(out, stateKeyWorkflowOutput); v != "" {
			workflowOutputs = append(workflowOutputs, v)
		}
	}

	merged.Set(stateKeyNodeOutputs, combinedOutputs)
	merged.Set(stateKeyNodeHandoffs, combinedHandoffs)
	if len(workflowOutputs) > 0 {
		merged.Set(stateKeyWorkflowOutput, strings.Join(workflowOutputs, "\n\n"))
	}
	return merged
}

func stringMapStateValue(state hxgraph.MapState, key string) map[string]string {
	if state == nil {
		return map[string]string{}
	}
	raw, _ := state.Get(key)
	src, _ := raw.(map[string]string)
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func putStringMapStateValue(state hxgraph.MapState, key, entryKey, entryValue string) hxgraph.MapState {
	next := state.Clone().(hxgraph.MapState)
	values := stringMapStateValue(next, key)
	values[entryKey] = entryValue
	next.Set(key, values)
	return next
}

func stringStateValue(state hxgraph.MapState, key string) string {
	if state == nil {
		return ""
	}
	raw, _ := state.Get(key)
	s, _ := raw.(string)
	return s
}

func setStringStateValue(state hxgraph.MapState, key, value string) hxgraph.MapState {
	next := state.Clone().(hxgraph.MapState)
	next.Set(key, value)
	return next
}

func renderTemplate(input string, state hxgraph.MapState) string {
	if input == "" {
		return ""
	}
	outputs := stringMapStateValue(state, stateKeyNodeOutputs)
	replacements := []string{
		"{{input}}", stringStateValue(state, stateKeyInput),
		"{{previous}}", "",
		"{{handoff_agent}}", "",
	}
	handoffs := stringMapStateValue(state, stateKeyNodeHandoffs)
	if len(handoffs) > 0 {
		var names []string
		for _, v := range handoffs {
			names = append(names, v)
		}
		sort.Strings(names)
		replacements[5] = strings.Join(names, ",")
	}
	if len(outputs) > 0 {
		var parts []string
		for _, v := range outputs {
			if v != "" {
				parts = append(parts, v)
			}
		}
		sort.Strings(parts)
		replacements[3] = strings.Join(parts, "\n\n")
	}
	for id, value := range outputs {
		replacements = append(replacements, "{{node."+id+"}}", value)
	}
	return strings.NewReplacer(replacements...).Replace(input)
}

func renderValue(v any, state hxgraph.MapState) any {
	switch x := v.(type) {
	case string:
		return renderTemplate(x, state)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = renderValue(val, state)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = renderValue(val, state)
		}
		return out
	default:
		return v
	}
}

func cloneMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return map[string]any{}
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func stringValue(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func stringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s := stringValue(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		parts := strings.Split(x, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

type agentrouterMessageAdapter struct {
	UserID     string
	Platform   string
	InstanceID string
	ChatID     string
	Content    string
	Metadata   map[string]string
}

func (m *agentrouterMessageAdapter) Message() *adapter.Message {
	return &adapter.Message{
		Platform:   adapter.PlatformAPI,
		UserID:     m.UserID,
		InstanceID: m.InstanceID,
		ChatID:     m.ChatID,
		Content:    m.Content,
		Metadata:   m.Metadata,
		Timestamp:  time.Now(),
	}
}

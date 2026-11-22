package flex

import (
	"context"
	"errors"
	"testing"
)

func testNode(name string, out any) NodeFunc {
	return func(_ context.Context, _ State) (State, error) {
		return State{name: out}, nil
	}
}

func TestInvokeLinearOrder(t *testing.T) {
	g := New("t", "a")
	g.AddNode("a", testNode("step", "a"))
	g.AddNode("b", testNode("step", "b"))
	g.AddNode("c", testNode("step", "c"))
	g.AddEdge("a", "b")
	g.AddEdge("b", "c")
	if err := g.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	state, err := g.Invoke(context.Background(), State{"in": 1})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	// 后节点覆盖前节点的同名键，最终为 c
	if state["step"] != "c" {
		t.Fatalf("step = %v, want c", state["step"])
	}
	if state["in"] != 1 {
		t.Fatalf("input key lost: %v", state)
	}
}

func TestInvokeNodeError(t *testing.T) {
	g := New("t", "a")
	g.AddNode("a", func(_ context.Context, _ State) (State, error) {
		return nil, errors.New("boom")
	})
	g.AddNode("b", testNode("step", "b"))
	g.AddEdge("a", "b")
	_ = g.Compile()
	if _, err := g.Invoke(context.Background(), State{}); err == nil {
		t.Fatal("expected node error")
	}
}

func TestCompileCycle(t *testing.T) {
	g := New("t", "a")
	g.AddNode("a", testNode("x", 1))
	g.AddNode("b", testNode("x", 2))
	g.AddEdge("a", "b")
	g.AddEdge("b", "a")
	if err := g.Compile(); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestCompileUnreachable(t *testing.T) {
	g := New("t", "a")
	g.AddNode("a", testNode("x", 1))
	g.AddNode("b", testNode("x", 2))
	// b 无入边也无出边：不可达
	if err := g.Compile(); err == nil {
		t.Fatal("expected unreachable node error")
	}
}

func TestInvokeTimeout(t *testing.T) {
	g := New("t", "a")
	g.AddNode("a", func(ctx context.Context, _ State) (State, error) {
		<-ctx.Done() // 阻塞直到 ctx 取消
		return State{}, ctx.Err()
	})
	_ = g.Compile()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	if _, err := g.Invoke(ctx, State{}); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestInvokeNotCompiled(t *testing.T) {
	g := New("t", "a")
	g.AddNode("a", testNode("x", 1))
	if _, err := g.Invoke(context.Background(), State{}); err == nil {
		t.Fatal("expected not-compiled error")
	}
}

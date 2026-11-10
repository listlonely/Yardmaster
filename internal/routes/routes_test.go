package routes

import "testing"

type fakeStrategy struct{ name string }

func (f fakeStrategy) Name() string              { return f.name }
func (f fakeStrategy) Order(seats []Seat) []Seat { return seats }

func TestRegisterFindList(t *testing.T) {
	Register(fakeStrategy{name: "test-order"})
	if Find("test-order") == nil {
		t.Fatal("注册后应可找到")
	}
	if Find("nope") != nil {
		t.Fatal("未注册策略应返回 nil（调用方回落 order）")
	}
	found := false
	for _, n := range List() {
		if n == "test-order" {
			found = true
		}
	}
	if !found {
		t.Fatalf("List 应包含已注册策略: %v", List())
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("重复注册应 panic（编程期错误）")
		}
	}()
	Register(fakeStrategy{name: "test-order"})
}

func TestStrategyContract(t *testing.T) {
	s := fakeStrategy{name: "t2"}
	in := []Seat{{ProviderID: "p"}, {ProviderID: "q"}}
	out := s.Order(in)
	if len(out) != 2 || out[0].ProviderID != "p" {
		t.Fatalf("Order 结果异常: %+v", out)
	}
	in[0].ProviderID = "changed" // 策略不得依赖入参切片之外的可变性
	_ = in
}

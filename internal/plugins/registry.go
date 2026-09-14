package plugins

// 描述注册的公开边界：Registry 持有插件的权威描述目录（纯数据），与真实
// 执行绑定（ExecutionBundle）严格分离。描述可以由任何宿主进程加载——包括
// 不执行任何出站调用的 Quoin 控制面——因此不会迫使描述宿主持有空执行器。

import "sort"

// Registry 是一个宿主进程内的插件契约目录：描述目录加上本进程真实提供
// 的执行绑定。它并发安全。
type Registry struct {
	impl registryState
}

// NewRegistry 返回空目录。
func NewRegistry() *Registry {
	return &Registry{}
}

// Descriptor 返回一个描述副本与是否存在标志。
func (r *Registry) Descriptor(id string) (Descriptor, bool) {
	state := r.impl.rlock()
	defer state.mu.RUnlock()
	descriptor, ok := state.descriptors[id]
	return descriptor, ok
}

// Descriptors 按 ID 稳定序返回全部描述。
func (r *Registry) Descriptors() []Descriptor {
	state := r.impl.rlock()
	defer state.mu.RUnlock()
	list := make([]Descriptor, 0, len(state.descriptors))
	for _, descriptor := range state.descriptors {
		list = append(list, descriptor)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// RegisterDescriptor 注册一个插件描述。描述必须通过全部静态校验：稳定
// ID、封闭 schema、能力声明与数据精确一致、全局唯一工具名。重复 ID 或
// 工具名被拒绝（注册错误是确定性的，不是运行时故障）。
func (r *Registry) RegisterDescriptor(descriptor Descriptor) error {
	state := r.impl.wlock()
	defer state.mu.Unlock()
	return state.registerDescriptor(descriptor)
}

// RegisterBundle 为一个已注册描述绑定本进程的真实执行实现。绑定被拒绝的
// 情形：描述不存在、位置未知、能力集合与非 nil 接口不精确相等、绑定了
// 描述未声明的能力、工具执行位置与绑定位置不一致、重复绑定。
func (r *Registry) RegisterBundle(bundle ExecutionBundle) error {
	state := r.impl.wlock()
	defer state.mu.Unlock()
	return state.registerBundle(bundle)
}

// Bundle 返回一个插件在本进程的执行绑定与是否存在标志。描述宿主（如不
// 执行出站调用的控制面）合法地没有绑定。
func (r *Registry) Bundle(id string) (ExecutionBundle, bool) {
	state := r.impl.rlock()
	defer state.mu.RUnlock()
	bundle, ok := state.bundles[id]
	return bundle, ok
}

// ToolOwner 返回一个模型工具名归属的插件 ID 与是否存在标志。
func (r *Registry) ToolOwner(name string) (string, bool) {
	state := r.impl.rlock()
	defer state.mu.RUnlock()
	owner, ok := state.toolOwners[name]
	return owner, ok
}

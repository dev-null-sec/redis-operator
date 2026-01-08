# Redis Operator 开发文档

## 📚 文档概述

本文档详细记录了 Redis Operator 项目的开发过程、架构设计、核心模块实现以及技术选型，为开发者提供全面的技术参考。

---

## 🎯 项目背景

### 痛点分析

在生产环境中，Kubernetes 原生的 StatefulSet 虽然能够管理有状态应用，但在管理 Redis 哨兵（Sentinel）集群时存在以下关键问题：

1. **应用层配置无法自动更新**
   - Pod 重启后 IP 发生漂移
   - Sentinel 配置文件中的 monitor IP 无法自动刷新
   - 需要手动介入更新配置，增加运维负担

2. **缺少故障转移自动化**
   - 主节点故障时需要手动触发 Sentinel 选举
   - 无法自动检测和恢复集群状态
   - 违背了 Kubernetes 自动化运维的初衷

3. **数据安全风险**
   - 删除 Pod 时未执行数据刷盘
   - 可能导致数据丢失
   - 缺少优雅终止机制

4. **配置管理复杂**
   - Redis 密码认证需要在多处配置
   - Sentinel 参数调整需要手动修改 ConfigMap
   - 缺少统一的声明式配置接口

### 解决方案

基于 Kubernetes Operator 模式开发自定义控制器，通过以下方式解决上述问题：

- ✅ **声明式 API**：通过 CRD 统一管理 Redis 和 Sentinel 配置
- ✅ **自动化调和**：Controller 持续监听并调和集群状态
- ✅ **动态拓扑感知**：自动检测 Pod IP 变化并更新 Sentinel 配置
- ✅ **优雅终止**：Finalizer 机制确保数据安全
- ✅ **故障转移**：Sentinel 自动检测主节点故障并选举新主

---

## 🏗️ 项目架构设计

### 整体架构

```
┌─────────────────────────────────────────────────────────────────┐
│                         用户层 (User Layer)                       │
│                                                                   │
│  kubectl apply -f rediscluster.yaml                              │
└─────────────────────────────┬───────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Kubernetes API Server                       │
│                                                                   │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │          RedisCluster CRD (db.redis.io/v1)              │   │
│  │  - Spec: Replicas, Password, SentinelConfig             │   │
│  │  - Status: ReadyReplicas, State                         │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────┬───────────────────────────────────┘
                              │ Watch/Informer
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                  Controller Runtime Framework                    │
│                                                                   │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │         RedisClusterReconciler (核心控制器)              │   │
│  │                                                           │   │
│  │  Reconcile Loop:                                         │   │
│  │  ┌─────────────────────────────────────────────────┐    │   │
│  │  │ 1. 获取 RedisCluster 资源                      │    │   │
│  │  │ 2. 检查 Finalizer 和删除标记                    │    │   │
│  │  │ 3. 管理 Service/StatefulSet/ConfigMap           │    │   │
│  │  │ 4. 配置 Redis 主从复制                           │    │   │
│  │  │ 5. 部署 Sentinel 集群                            │    │   │
│  │  │ 6. 更新集群状态                                  │    │   │
│  │  └─────────────────────────────────────────────────┘    │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                   │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │         Workqueue (事件队列)                             │   │
│  │  - CRD 创建/更新/删除事件                                │   │
│  │  - StatefulSet 变化事件                                  │   │
│  │  - Pod 变化事件                                          │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────┬───────────────────────────────────┘
                              │ Client-Go
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Kubernetes Resource Layer                     │
│                                                                   │
│  ┌─────────────┐ ┌─────────────┐ ┌─────────────┐               │
│  │ StatefulSet │ │   Service   │ │  ConfigMap  │               │
│  │  (Redis)    │ │ (Headless)  │ │ (Sentinel)   │               │
│  └─────────────┘ └─────────────┘ └─────────────┘               │
│                                                                   │
│  ┌─────────────┐ ┌─────────────┐                                │
│  │ StatefulSet │ │   Service   │                                │
│  │ (Sentinel)  │ │ (Headless)  │                                │
│  └─────────────┘ └─────────────┘                                │
└─────────────────────────────────────────────────────────────────┘
```

### 数据流向

```
用户提交 CRD
    │
    ├─> Kubernetes API Server
    │       │
    │       ├─> 持久化到 etcd
    │       │
    │       └─> 触发 Watch 事件
    │
    └─> Controller Informer
            │
            ├─> 将事件加入 Workqueue
            │
            └─> Reconcile Loop
                    │
                    ├─> 获取最新资源状态
                    │
                    ├─> 对比期望状态 vs 实际状态
                    │
                    ├─> 执行调和逻辑
                    │       │
                    │       ├─> 创建/更新 Service
                    │       │
                    │       ├─> 创建/更新 StatefulSet
                    │       │
                    │       ├─> 创建/更新 ConfigMap
                    │       │
                    │       ├─> 配置 Redis 主从
                    │       │
                    │       ├─> 部署 Sentinel
                    │       │
                    │       └─> 更新 Status
                    │
                    └─> 返回调和结果
```

---

## 🔧 核心模块详解

### 1. CRD 定义模块 (`api/v1/`)

#### RedisCluster 结构

**文件**: `api/v1/rediscluster_types.go`

```go
type RedisCluster struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`

    Spec   RedisClusterSpec   `json:"spec"`
    Status RedisClusterStatus `json:"status"`
}
```

**设计要点**:
- 使用 `+kubebuilder` 标记添加验证规则
- `+kubebuilder:validation:Minimum=1` 确保副本数至少为 1
- `+kubebuilder:subresource:status` 启用状态子资源
- 所有字段都提供详细的 godoc 注释

#### SentinelConfig 结构

```go
type SentinelConfig struct {
    Replicas                       int32 `json:"replicas,omitempty"`
    Port                           int32 `json:"port,omitempty"`
    Quorum                         int32 `json:"quorum,omitempty"`
    DownAfterMilliseconds          int32 `json:"downAfterMilliseconds,omitempty"`
    FailoverTimeoutMilliseconds    int32 `json:"failoverTimeoutMilliseconds,omitempty"`
    Password                       string `json:"password,omitempty"`
}
```

**默认值策略**:
- 使用 `+kubebuilder:default` 标记设置默认值
- 在 Controller 中进行 nil 检查并提供运行时默认值
- 确保零值也能正常工作

### 2. Controller 核心逻辑 (`internal/controller/`)

#### Reconcile 循环

**文件**: `internal/controller/rediscluster_controller.go:42-127`

```go
func (r *RedisClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. 获取资源
    // 2. 检查删除标记
    // 3. 添加 Finalizer
    // 4. 管理 Service
    // 5. 管理 StatefulSet
    // 6. 调和 Sentinel
    // 7. 配置拓扑
    // 8. 更新状态
}
```

**调和策略**:
- **水平触发 (Level-Triggered)**: 每次调和都基于当前状态，而非边沿触发
- **幂等性**: 多次执行相同操作结果一致
- **重试机制**: 错误时返回 `ctrl.Result{RequeueAfter: ...}`
- **错误容忍**: 单个节点失败不影响整体调和

#### Finalizer 机制

**文件**: `internal/controller/rediscluster_controller.go:249-348`

```go
const redisClusterFinalizer = "rediscluster.db.redis.io/finalizer"

func (r *RedisClusterReconciler) handleDeletion(ctx context.Context, redis *dbv1.RedisCluster) (ctrl.Result, error) {
    // 1. 检查 Finalizer
    // 2. 执行 BGSAVE 保存数据
    // 3. 注销 Sentinel 监控
    // 4. 移除 Finalizer
    // 5. 允许删除
}
```

**设计要点**:
- 删除前拦截，执行清理逻辑
- 连接所有 Redis Pod 执行 `BGSAVE`
- 失败时返回重试，确保数据安全
- 完成清理后才移除 Finalizer

#### Sentinel 管理

**文件**: `internal/controller/rediscluster_controller.go:359-596`

**核心函数**:

1. **reconcileSentinel**: 主调和函数
   - 协调 ConfigMap、Service、StatefulSet
   - 按顺序创建资源，确保依赖关系

2. **reconcileSentinelConfigMap**: 管理 ConfigMap
   - 动态生成 `sentinel.conf`
   - 检测配置变化并自动更新

3. **generateSentinelConfig**: 生成配置文件
   - 使用 K8s DNS 解析 Master 地址
   - 注入密码认证配置
   - 支持自定义参数

**配置模板**:

```conf
# Generated by Redis Operator
port 26379
dir /tmp
daemonize no
pidfile /tmp/sentinel.pid
logfile ""

monitor mymaster redis-0.redis-cluster.default.svc.cluster.local 6379 2
down-after-milliseconds mymaster 5000
failover-timeout mymaster 10000
parallel-syncs mymaster 1

auth-pass mymaster your-password
```

#### 动态拓扑配置

**文件**: `internal/controller/rediscluster_controller.go:216-261`

```go
func (r *RedisClusterReconciler) reconcileClusterTopology(ctx context.Context, redisCR *dbv1.RedisCluster) error {
    // 1. 遍历所有 Pod
    // 2. redis-0 设为 Master
    // 3. redis-1..N 设为 Slave
    // 4. 使用密码认证
}
```

**技术细节**:
- 使用 K8s FQDN (Fully Qualified Domain Name)
- 格式: `pod-name.service-name.namespace.svc.cluster.local`
- Pod 重启后 DNS 自动更新，无需手动修改配置

### 3. RBAC 权限管理

**文件**: `internal/controller/rediscluster_controller.go:33-39`

```go
// +kubebuilder:rbac:groups=db.redis.io,resources=redisclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
```

**最小权限原则**:
- 只授予必要的权限
- 区分读、写、列表操作
- 支持资源级别权限控制

---

## 📂 关键文件说明

### API 定义层

| 文件 | 行数 | 作用 |
|------|------|------|
| `api/v1/groupversion_info.go` | ~40 | 定义 API 组和版本信息 `db.redis.io/v1` |
| `api/v1/rediscluster_types.go` | ~80 | 定义 CRD 数据结构（Spec、Status） |
| `api/v1/zz_generated.deepcopy.go` | ~130 | 自动生成的深拷贝方法，避免引用传递问题 |

### Controller 层

| 文件 | 行数 | 作用 |
|------|------|------|
| `internal/controller/rediscluster_controller.go` | ~600 | 核心调和逻辑实现 |
| `internal/controller/suite_test.go` | ~50 | 测试套件初始化 |
| `internal/controller/rediscluster_controller_test.go` | ~100 | 单元测试（可扩展） |

### 配置文件层

| 目录 | 作用 |
|------|------|
| `config/crd/bases/` | CRD YAML 定义，由 `controller-gen` 自动生成 |
| `config/rbac/` | RBAC 权限配置（Role、RoleBinding、ServiceAccount） |
| `config/manager/` | Manager 部署配置，包含 Prometheus 监控 |
| `config/default/` | 默认部署配置，通过 Kustomize 组装所有资源 |
| `config/samples/` | 示例 YAML，供用户参考 |

### 入口和构建

| 文件 | 作用 |
|------|------|
| `cmd/main.go` | 程序入口，初始化 Manager 并注册 Controller |
| `Makefile` | 构建脚本，封装 `go build`、`docker build`、`kustomize` 等命令 |
| `PROJECT` | Kubebuilder 项目元数据，记录项目配置 |

---

## 🚀 开发流程

### 1. 环境准备

```bash
# 安装 Go 1.24.6+
# 安装 Docker
# 安装 kubectl
# 准备 Kubernetes 集群（Kind/Minikube/云厂商）

# 克隆项目
git clone https://github.com/your-username/redis-operator.git
cd redis-operator

# 安装依赖
go mod download
```

### 2. 本地开发

```bash
# 启动本地 Kind 集群（可选）
kind create cluster --name redis-operator-dev

# 运行 Operator（连接到远程或本地集群）
make run

# 重新生成代码（修改 CRD 后必须执行）
make manifests generate

# 格式化和检查
make fmt vet
```

### 3. 测试

```bash
# 单元测试
make test

# 端到端测试（需要 Kind 集群）
make test-e2e

# 查看测试覆盖率
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

### 4. 构建和部署

```bash
# 构建镜像
make docker-build IMG=<registry>/redis-operator:tag

# 推送镜像
make docker-push IMG=<registry>/redis-operator:tag

# 部署到集群
make deploy IMG=<registry>/redis-operator:tag

# 创建测试资源
kubectl apply -f config/samples/db_v1_rediscluster.yaml
```

### 5. 调试技巧

```bash
# 查看 Controller 日志
kubectl logs -n redis-operator-system -l app.kubernetes.io/name=redis-operator -f

# 查看事件
kubectl get events --sort-by='.lastTimestamp'

# 调试单个 Reconcile 循环
# 在代码中添加 log.Log.SetLogger(log.NewDelegatingLogger(log.NewNullLogger()))

# 查看 CRD 实例
kubectl get rediscluster -o yaml

# 查看状态
kubectl describe rediscluster redis-sample
```

---

## 🔍 技术选型

### 为什么选择 Kubebuilder？

| 特性 | 优势 |
|------|------|
| **脚手架工具** | 快速创建项目结构，减少重复工作 |
| **代码生成** | 自动生成 CRD、RBAC、DeepCopy 等样板代码 |
| **标准化** | 遵循 Kubernetes Operator 最佳实践 |
| **生态支持** | 官方维护，社区活跃，文档完善 |

### 为什么选择 Controller Runtime？

| 特性 | 优势 |
|------|------|
| **高级抽象** | 封装了底层 Client-Go 复杂性 |
| **Informer 机制** | 自动缓存和 Watch 资源变化 |
| **Workqueue** | 内置事件队列，支持速率限制和重试 |
| **Manager 模式** | 统一管理多个 Controller 和 Webhook |

### 为什么使用 StatefulSet？

| 特性 | 优势 |
|------|------|
| **稳定标识** | Pod 名称固定，DNS 稳定 |
| **有序部署** | 按顺序启动 Pod，确保 Master 先于 Slave |
| **持久化存储** | PVC 与 Pod 绑定，删除后数据不丢失 |
| **滚动更新** | 支持灰度发布和回滚 |

### 为什么使用 ConfigMap 存储配置？

| 特性 | 优势 |
|------|------|
| **版本控制** | 配置变更可审计、可回滚 |
| **热更新** | 更新 ConfigMap 后自动挂载到 Pod |
| **解耦** | 配置与镜像分离，灵活调整参数 |
| **共享**：多个 Pod 可共享同一配置 |

---

## 🧪 测试策略

### 单元测试

**目标**: 测试单个函数逻辑

```go
func TestReconcileClusterTopology(t *testing.T) {
    // 创建 Fake Client
    // 初始化测试环境
    // 调用 reconcileClusterTopology
    // 验证结果
}
```

### 集成测试

**目标**: 测试 Controller 与 K8s API 交互

```go
func TestRedisClusterReconciler(t *testing.T) {
    // 使用 envtest 创建本地 API Server
    // 注册 CRD
    // 创建 RedisCluster 实例
    // 验证资源创建
}
```

### 端到端测试

**目标**: 测试完整的工作流

**文件**: `test/e2e/e2e_test.go`

```go
var _ = Describe("RedisCluster", func() {
    Context("When creating RedisCluster", func() {
        It("Should create Redis and Sentinel pods", func() {
            // 部署 Operator
            // 创建 RedisCluster
            // 等待就绪
            // 验证 Pod 数量和状态
        })
    })
})
```

---

## 📈 性能优化

### 1. 减少调和次数

```go
// 只在配置变更时更新
if cm.Data["sentinel.conf"] != sentinelConf {
    cm.Data["sentinel.conf"] = sentinelConf
    return r.Update(ctx, cm)
}
```

### 2. 批量操作

```go
// 使用 Client 批量创建资源
for _, resource := range resources {
    if err := r.Create(ctx, resource); err != nil {
        return err
    }
}
```

### 3. 错误处理优化

```go
// 单个节点失败不影响整体
for _, pod := range pods {
    if err := configurePod(pod); err != nil {
        logger.Error(err, "配置失败，继续下一个", "pod", pod.Name)
        continue
    }
}
```

### 4. 指数退避重试

```go
// 失败后延迟重试，避免频繁调用
return ctrl.Result{RequeueAfter: time.Second * 10}, nil
```

---

## 🚧 已知限制和未来规划

### 当前限制

1. **不支持集群模式**: 仅支持哨兵模式，不支持 Redis Cluster
2. **单命名空间**: 未实现跨命名空间的管理
3. **备份恢复**: 缺少自动备份和恢复机制
4. **监控告警**: 未集成 Prometheus 告警规则

### 未来规划

#### Phase 1: 增强功能
- [ ] 支持密码热更新（无需重启 Pod）
- [ ] 增加健康检查探针（Liveness/Readiness）
- [ ] 实现 Pod 优雅关闭（PreStop Hook）
- [ ] 支持持久卷类型选择（SSD/HDD）

#### Phase 2: 运维增强
- [ ] 集成 Prometheus Exporter
- [ ] 实现自动备份（定时 RDB 快照）
- [ ] 支持在线扩容（Slot 迁移）
- [ ] 添加性能指标面板（Grafana Dashboard）

#### Phase 3: 高级特性
- [ ] 支持 Redis Cluster 模式
- [ ] 实现多可用区部署
- [ ] 集成日志收集（ELK/Loki）
- [ ] 支持灾备和跨区域复制

#### Phase 4: 生态集成
- [ ] Helm Chart 发布
- [ ] Operator Hub 认证
- [ ] OLM (Operator Lifecycle Manager) 支持
- [ ] Open Policy Agent (OPA) 策略集成

---

## 📚 参考资料

### 官方文档
- [Kubebuilder 官方文档](https://book.kubebuilder.io/)
- [Controller Runtime 文档](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [Redis Sentinel 官方文档](https://redis.io/docs/manual/sentinel/)
- [Kubernetes API 约定](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)

### 最佳实践
- [Kubernetes Operator 模式](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Go 语言最佳实践](https://go.dev/doc/effective_go)
- [云原生应用白皮书](https://www.cncf.io/wp-content/uploads/2021/05/CNCF_Business_Value_Slides_Final.pdf)

### 社区资源
- [Operator Builder](https://operatorbuilder.io/)
- [Kubebuilder Google Group](https://groups.google.com/g/kubebuilder)
- [CNCF Slack - #kubebuilder 频道](https://cloud-native.slack.com/)

---

## 🤝 贡献指南

### 提交代码

1. Fork 项目
2. 创建特性分支: `git checkout -b feature/your-feature`
3. 提交代码: `git commit -m 'Add some feature'`
4. 推送分支: `git push origin feature/your-feature`
5. 提交 Pull Request

### 代码审查清单

- [ ] 遵循 Go 代码规范
- [ ] 添加完整的函数注释
- [ ] 包含单元测试
- [ ] 通过 `make fmt vet` 检查
- [ ] 更新相关文档

### Issue 模板

```markdown
## 问题描述
简要描述遇到的问题

## 复现步骤
1. 执行操作...
2. 观察到...
3. 期望...

## 环境信息
- Kubernetes 版本:
- Operator 版本:
- Redis 版本:

## 日志输出
\```
粘贴相关日志
\```
```

---

## 📄 许可证

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

---

<div align="center">

**🎉 感谢你对 Redis Operator 项目的关注和支持！**

如有问题，欢迎提交 Issue 或 Pull Request

</div>

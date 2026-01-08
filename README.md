# Redis Operator

基于 Kubebuilder 的 Redis 哨兵集群高可用 Operator，自动管理 Redis Sentinel 故障转移和集群拓扑。

## 功能特性

- **CRD 定义**：通过 RedisCluster CRD 统一管理 Redis 和 Sentinel 配置，支持 Replicas、Password、SentinelConfig 等字段
- **自动调和**：Reconcile 循环持续对比期望状态和实际状态，基于 Level-Triggered 机制
- **Sentinel 集群**：自动创建独立的 Sentinel StatefulSet、ConfigMap 和 Service
- **动态拓扑**：Pod 重启后自动更新 Sentinel 配置中的 monitor IP（使用 K8s DNS）
- **优雅终止**：Finalizer 机制拦截删除事件，执行 BGSAVE 后再删除资源
- **密码认证**：支持 Redis 和 Sentinel 双重密码保护

## 架构设计

```
用户提交 RedisCluster CRD
    ↓
Controller 监听 CRD 变化
    ↓
Reconcile 循环：
  1. 创建 Headless Service（提供稳定 DNS）
  2. 创建 Redis StatefulSet（redis-0..N）
  3. 创建 Sentinel ConfigMap（sentinel.conf）
  4. 创建 Sentinel StatefulSet（哨兵集群）
  5. 配置 Redis 主从复制（redis-0 为 Master）
  6. 更新 CRD Status
```

**核心设计**：
- StatefulSet 部署 Redis，保证 Pod 名称和存储稳定
- Headless Service 提供稳定的 DNS 记录
- ConfigMap 存储 sentinel.conf，支持动态更新
- K8s FQDN（pod-name.service-name.ns.svc.cluster.local）代替 Pod IP
- Finalizer 实现删除前数据刷盘

## 快速开始

### 前置要求

- Go 1.24.6+
- Docker 17.03+
- kubectl 1.11.3+
- Kubernetes 1.11.3+

### 部署

```bash
# 安装 CRD
make install

# 构建并推送镜像
make docker-build docker-push IMG=<registry>/redis-operator:v1.0.0

# 部署 Operator
make deploy IMG=<registry>/redis-operator:v1.0.0

# 创建 Redis 集群
kubectl apply -f config/samples/db_v1_rediscluster.yaml

# 查看状态
kubectl get rediscluster
kubectl get pods -l app=redis-sample
```

## 配置说明

### CRD 定义

```yaml
apiVersion: db.redis.io/v1
kind: RedisCluster
metadata:
  name: redis-cluster
spec:
  replicas: 3                    # Redis 节点数
  image: "redis:7.0"
  port: 6379
  storageSize: "1Gi"
  password: "your-password"      # Redis 密码

  enableSentinel: true           # 启用哨兵模式
  sentinelConfig:
    replicas: 3                   # Sentinel 副本数
    port: 26379
    quorum: 2                     # 故障转移投票数
    downAfterMilliseconds: 5000
    failoverTimeoutMilliseconds: 10000
    password: "your-password"
```

**字段说明**：

| 字段 | 类型 | 说明 |
|------|------|------|
| replicas | int32 | Redis 节点数量（最小 1） |
| image | string | Redis 镜像地址 |
| port | int32 | Redis 端口（默认 6379） |
| storageSize | string | 存储卷大小 |
| password | string | Redis 认证密码 |
| enableSentinel | bool | 是否启用哨兵模式 |
| sentinelConfig | object | 哨兵详细配置 |

**Status 字段**：

```yaml
status:
  readyReplicas: 3
  state: "Running"
```

## 代码架构

### 项目结构

```
redis-operator/
├── api/v1/
│   ├── rediscluster_types.go       # CRD 定义
│   └── zz_generated.deepcopy.go    # 深拷贝代码
│
├── internal/controller/
│   └── rediscluster_controller.go  # Controller 实现
│       ├── Reconcile()             # 主调和循环
│       ├── handleDeletion()        # Finalizer 逻辑
│       ├── reconcileSentinel()     # Sentinel 管理
│       └── reconcileClusterTopology()  # 主从配置
│
└── cmd/main.go                     # 程序入口
```

### Reconcile 循环

每次 CRD 变化触发调和：

```go
func (r *RedisClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. 获取 RedisCluster 资源
    redis := &dbv1.RedisCluster{}
    r.Get(ctx, req.NamespacedName, redis)

    // 2. 检查删除标记
    if !redis.DeletionTimestamp.IsZero() {
        return r.handleDeletion(ctx, redis)
    }

    // 3. 添加 Finalizer
    if !containsString(redis.Finalizers, redisClusterFinalizer) {
        redis.Finalizers = append(redis.Finalizers, redisClusterFinalizer)
        r.Update(ctx, redis)
    }

    // 4. 管理 Service 和 StatefulSet
    r.constructService(redis)
    r.constructStatefulSet(redis)

    // 5. 调和 Sentinel
    if redis.Spec.EnableSentinel {
        r.reconcileSentinel(ctx, redis)
    }

    // 6. 配置主从复制
    r.reconcileClusterTopology(ctx, redis)

    // 7. 更新 Status
    redis.Status.ReadyReplicas = sts.Status.ReadyReplicas
    r.Status().Update(ctx, redis)

    return ctrl.Result{}, nil
}
```

### Finalizer 实现

删除前执行数据刷盘：

```go
const redisClusterFinalizer = "rediscluster.db.redis.io/finalizer"

func (r *RedisClusterReconciler) handleDeletion(ctx context.Context, redis *dbv1.RedisCluster) {
    // 1. 连接所有 Redis Pod
    podList := &corev1.PodList{}
    r.List(ctx, podList, client.InNamespace(redis.Namespace))

    // 2. 执行 BGSAVE
    for _, pod := range podList.Items {
        rdb := goRedis.NewClient(&goRedis.Options{
            Addr: podDNS + ":6379",
            Password: redis.Spec.Password,
        })
        rdb.BgSave(ctx)  // 后台保存，不阻塞
    }

    // 3. 移除 Finalizer，允许删除
    redis.Finalizers = removeString(redis.Finalizers, redisClusterFinalizer)
    r.Update(ctx, redis)
}
```

### Sentinel 管理

动态生成配置文件：

```go
func (r *RedisClusterReconciler) reconcileSentinel(ctx context.Context, redis *dbv1.RedisCluster) {
    // 1. 生成 sentinel.conf
    masterDNS := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", redis.Name, redis.Name, redis.Namespace)
    sentinelConf := fmt.Sprintf(`
port 26379
monitor mymaster %s 6379 2
down-after-milliseconds mymaster 5000
failover-timeout mymaster 10000
auth-pass mymaster %s
`, masterDNS, redis.Spec.Password)

    // 2. 创建 ConfigMap
    cm := &corev1.ConfigMap{
        Data: map[string]string{"sentinel.conf": sentinelConf},
    }
    r.Create(ctx, cm)

    // 3. 创建 Sentinel StatefulSet
    sts := &appsv1.StatefulSet{
        Spec: appsv1.StatefulSetSpec{
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    Containers: []corev1.Container{
                        {
                            Command: []string{"redis-sentinel", "/etc/sentinel/sentinel.conf"},
                        },
                    },
                },
            },
        },
    }
    r.Create(ctx, sts)
}
```

### 主从配置

使用 Redis 客户端配置复制：

```go
func (r *RedisClusterReconciler) reconcileClusterTopology(ctx context.Context, redis *dbv1.RedisCluster) {
    masterDNS := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", redis.Name, redis.Name, redis.Namespace)

    for i := 0; i < int(redis.Spec.Replicas); i++ {
        podDNS := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", redis.Name, i, redis.Name, redis.Namespace)

        rdb := goRedis.NewClient(&goRedis.Options{
            Addr: podDNS + ":6379",
            Password: redis.Spec.Password,
        })

        if i == 0 {
            rdb.SlaveOf(ctx, "NO", "ONE")  // Master
        } else {
            rdb.SlaveOf(ctx, masterDNS, "6379")  // Slave
        }
    }
}
```

### RBAC 权限

```go
// +kubebuilder:rbac:groups=db.redis.io,resources=redisclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;pods,verbs=get;list;watch;create;update;patch;delete
```

## 运维操作

```bash
# 扩容
kubectl patch rediscluster redis-sample -p '{"spec":{"replicas":5}}' --type=merge

# 修改密码
kubectl patch rediscluster redis-sample -p '{"spec":{"password":"new-pass"}}' --type=merge
kubectl delete pod -l app=redis-sample

# 查看日志
kubectl logs -n redis-operator-system -l app.kubernetes.io/name=redis-operator -f

# 删除集群（触发 Finalizer）
kubectl delete rediscluster redis-sample
```

## 监控

Operator 暴露 Prometheus 指标：

```bash
kubectl port-forward -n redis-operator-system svc/redis-operator-controller-manager-metrics-service 8443:8443
curl http://localhost:8443/metrics
```

关键指标：
- `controller_runtime_reconcile_total`：调和次数
- `controller_runtime_reconcile_errors`：调和错误
- `workqueue_depth`：队列深度

## 开发

```bash
# 本地运行
make install
make run

# 测试
make test
make test-e2e

# 代码生成
make manifests generate
make fmt
make vet
```

## 已知限制

- 仅支持哨兵模式，不支持 Redis Cluster
- 修改密码后需要手动重启 Pod
- 单命名空间管理

## 未来计划

- 密码热更新
- 健康检查探针
- Prometheus Exporter 集成
- 自动备份功能

## License

Apache License 2.0

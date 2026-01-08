package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dbv1 "redis.io/operator/api/v1"
	goRedis "github.com/redis/go-redis/v9"
)

const (
	redisClusterFinalizer = "rediscluster.db.redis.io/finalizer"
)

// RedisClusterReconciler 负责协调 RedisCluster 资源的状态
type RedisClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC 权限声明
// +kubebuilder:rbac:groups=db.redis.io,resources=redisclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.redis.io,resources=redisclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile 实现 Kubernetes 控制器的协调逻辑
func (r *RedisClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. 获取 RedisCluster 资源
	redis := &dbv1.RedisCluster{}
	err := r.Get(ctx, req.NamespacedName, redis)
	if err != nil {
		if errors.IsNotFound(err) {
			// 资源已被删除，无需处理
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. 检查是否正在删除，执行 Finalizer 逻辑
	if !redis.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, redis)
	}

	// 3. 添加 Finalizer（如果还没有）
	if !containsString(redis.Finalizers, redisClusterFinalizer) {
		redis.Finalizers = append(redis.Finalizers, redisClusterFinalizer)
		if err := r.Update(ctx, redis); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 2. 确保 Headless Service 存在
	// 为每个 Redis 节点提供稳定的网络标识
	svc := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: redis.Name, Namespace: redis.Namespace}, svc)
	if err != nil && errors.IsNotFound(err) {
		// Service 不存在，创建新的
		newSvc := r.constructService(redis)
		logger.Info("创建 Headless Service", "name", newSvc.Name)
		if err := r.Create(ctx, newSvc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 3. 确保 StatefulSet 存在
	sts := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: redis.Name, Namespace: redis.Namespace}, sts)
	if err != nil && errors.IsNotFound(err) {
		// StatefulSet 不存在，创建新的
		newSts := r.constructStatefulSet(redis)
		logger.Info("创建 StatefulSet", "name", newSts.Name)
		if err := r.Create(ctx, newSts); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	} else if err == nil {
		// 检查并更新副本数
		if *sts.Spec.Replicas != redis.Spec.Replicas {
			sts.Spec.Replicas = &redis.Spec.Replicas
			if err := r.Update(ctx, sts); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// 4. 如果启用哨兵模式，创建 Sentinel 相关资源
	if redis.Spec.EnableSentinel {
		if err := r.reconcileSentinel(ctx, redis); err != nil {
			logger.Error(err, "调和 Sentinel 失败")
			return ctrl.Result{}, err
		}
	}

// 5. 集群拓扑配置
	// 当启用哨兵模式且所有节点就绪时执行
	if redis.Spec.EnableSentinel && sts.Status.ReadyReplicas == redis.Spec.Replicas {
		if err := r.reconcileClusterTopology(ctx, redis); err != nil {
			logger.Error(err, "配置集群拓扑失败，将在下次重试")
			// 不返回错误，让调和继续，避免频繁重试
		}
	}

	// 6. 更新 RedisCluster 状态
	redis.Status.ReadyReplicas = sts.Status.ReadyReplicas
	redis.Status.State = "Running"

	// 7. 如果启用哨兵模式，检测故障转移和健康状态
	if redis.Spec.EnableSentinel && sts.Status.ReadyReplicas == redis.Spec.Replicas {
		if err := r.checkFailoverAndHealth(ctx, redis); err != nil {
			logger.Error(err, "故障转移和健康检查失败")
			// 不返回错误，避免频繁重试
		}
	}

	if err := r.Status().Update(ctx, redis); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// constructService 创建 Headless Service
func (r *RedisClusterReconciler) constructService(redis *dbv1.RedisCluster) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redis.Name,
			Namespace: redis.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: "redis",
					Port: redis.Spec.Port,
				},
			},
			Selector: map[string]string{
				"app": redis.Name,
			},
			// ClusterIP: None 创建 Headless Service
			// 为每个 Pod 分配独立的 DNS 记录
			ClusterIP: "None",
		},
	}
	// 设置 OwnerReference，确保 Service 随 RedisCluster 一起清理
	ctrl.SetControllerReference(redis, svc, r.Scheme)
	return svc
}

// constructStatefulSet 创建 Redis StatefulSet
func (r *RedisClusterReconciler) constructStatefulSet(redis *dbv1.RedisCluster) *appsv1.StatefulSet {
	replicas := redis.Spec.Replicas
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redis.Name,
			Namespace: redis.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": redis.Name},
			},
			ServiceName: redis.Name,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": redis.Name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "redis",
							Image: redis.Spec.Image,
							Ports: []corev1.ContainerPort{{ContainerPort: redis.Spec.Port}},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data"},
							},
						},
					},
				},
			},
			// VolumeClaimTemplates 为每个 Pod 自动创建 PVC
			// 保证数据持久化存储
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(redis.Spec.StorageSize),
							},
						},
					},
				},
			},
		},
	}
	ctrl.SetControllerReference(redis, sts, r.Scheme)
	return sts
}

// SetupWithManager 设置 Controller 并注册到 Manager
func (r *RedisClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1.RedisCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

// reconcileClusterTopology 配置 Redis 主从复制
func (r *RedisClusterReconciler) reconcileClusterTopology(ctx context.Context, redisCR *dbv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// 使用 K8s DNS 名称连接 Redis Pod
	// 格式：pod-name.service-name.namespace.svc.cluster.local

	// 指定第一个 Pod 为 Master
	masterPodName := fmt.Sprintf("%s-0", redisCR.Name)
	masterDNS := fmt.Sprintf("%s.%s.%s.svc.cluster.local", masterPodName, redisCR.Name, redisCR.Namespace)

	// 遍历所有副本，配置主从关系
	for i := 0; i < int(redisCR.Spec.Replicas); i++ {
		podName := fmt.Sprintf("%s-%d", redisCR.Name, i)
		podDNS := fmt.Sprintf("%s.%s.%s.svc.cluster.local", podName, redisCR.Name, redisCR.Namespace)

		// 创建 Redis 客户端连接（使用密码认证）
		rdb := goRedis.NewClient(&goRedis.Options{
			Addr:        podDNS + ":6379",
			Password:    redisCR.Spec.Password,
			DialTimeout: 2 * time.Second,
			MaxRetries:  3,
		})
		defer rdb.Close()

		if i == 0 {
			// 配置为 Master 节点
			if err := rdb.SlaveOf(ctx, "NO", "ONE").Err(); err != nil {
				logger.Error(err, "设置 Master 失败", "pod", podName)
				// 继续尝试其他节点，不因为单个节点失败而返回错误
			} else {
				logger.Info("成功设置 Master 节点", "pod", podName)
			}
		} else {
			// 配置为 Slave 节点，指向 Master
			if err := rdb.SlaveOf(ctx, masterDNS, "6379").Err(); err != nil {
				logger.Error(err, "设置 Slave 失败", "pod", podName, "master", masterDNS)
				// 继续尝试其他节点
			} else {
				logger.Info("成功设置 Slave 节点", "pod", podName)
			}
		}
	}

	return nil
}

// handleDeletion 处理 RedisCluster 的删除逻辑，实现优雅终止
func (r *RedisClusterReconciler) handleDeletion(ctx context.Context, redis *dbv1.RedisCluster) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 检查是否包含我们的 Finalizer
	if !containsString(redis.Finalizers, redisClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	logger.Info("开始执行优雅终止流程", "name", redis.Name)

	// 1. 尝试保存 Redis 数据（执行 SAVE 命令）
	if err := r.saveRedisDataBeforeDeletion(ctx, redis); err != nil {
		logger.Error(err, "保存 Redis 数据失败，重试中...")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// 2. 如果启用了哨兵模式，注销 Sentinel 监控
	if redis.Spec.EnableSentinel {
		if err := r.removeSentinelMonitoring(ctx, redis); err != nil {
			logger.Error(err, "注销 Sentinel 监控失败，重试中...")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// 3. 移除 Finalizer，允许 Kubernetes 删除资源
	logger.Info("清理完成，移除 Finalizer", "name", redis.Name)
	redis.Finalizers = removeString(redis.Finalizers, redisClusterFinalizer)
	if err := r.Update(ctx, redis); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// saveRedisDataBeforeDeletion 在删除前保存 Redis 数据到磁盘
func (r *RedisClusterReconciler) saveRedisDataBeforeDeletion(ctx context.Context, redis *dbv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// 获取所有 Redis Pod
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(redis.Namespace),
		client.MatchingLabels(map[string]string{"app": redis.Name}),
	}
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return fmt.Errorf("获取 Pod 列表失败: %w", err)
	}

	// 尝试连接每个 Pod 并执行 SAVE 命令
	for _, pod := range podList.Items {
		podDNS := fmt.Sprintf("%s.%s.%s.svc.cluster.local", pod.Name, redis.Name, redis.Namespace)

		rdb := goRedis.NewClient(&goRedis.Options{
			Addr:        podDNS + ":6379",
			Password:    redis.Spec.Password,
			DialTimeout: 2 * time.Second,
		})

		// 执行 BGSAVE（后台保存，不阻塞）
		if err := rdb.BgSave(ctx).Err(); err != nil {
			logger.Info("执行 BGSAVE 失败", "pod", pod.Name, "error", err)
			// 继续处理其他节点，不因为单个节点失败而停止
		} else {
			logger.Info("成功执行 BGSAVE", "pod", pod.Name)
		}
		rdb.Close()
	}

	return nil
}

// removeSentinelMonitoring 从 Sentinel 中注销监控
func (r *RedisClusterReconciler) removeSentinelMonitoring(ctx context.Context, redis *dbv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// 1. 获取所有 Sentinel Pod
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(redis.Namespace),
		client.MatchingLabels(map[string]string{
			"app":        redis.Name,
			"component": "sentinel",
		}),
	}

	if err := r.List(ctx, podList, listOpts...); err != nil {
		return fmt.Errorf("获取 Sentinel Pod 列表失败: %w", err)
	}

	if len(podList.Items) == 0 {
		logger.Info("没有找到 Sentinel Pod，跳过注销", "name", redis.Name)
		return nil
	}

	// 2. 连接每个 Sentinel 并移除监控
	sentinelPort := int32(26379)
	if redis.Spec.SentinelConfig != nil && redis.Spec.SentinelConfig.Port != 0 {
		sentinelPort = redis.Spec.SentinelConfig.Port
	}

	masterName := "mymaster"
	successCount := 0

	for _, pod := range podList.Items {
		sentinelDNS := fmt.Sprintf("%s.%s-sentinel.%s.svc.cluster.local",
			pod.Name, redis.Name, redis.Namespace)

		// 创建 Sentinel 客户端
		sentinelClient := goRedis.NewClient(&goRedis.Options{
			Addr:        fmt.Sprintf("%s:%d", sentinelDNS, sentinelPort),
			Password:    redis.Spec.Password,
			DialTimeout: 2 * time.Second,
			MaxRetries:  3,
		})

		// 执行 SENTINEL REMOVE 命令
		result, err := sentinelClient.Do(ctx, "SENTINEL", "REMOVE", masterName).Result()
		if err != nil {
			logger.Info("Sentinel REMOVE 失败", "pod", pod.Name, "error", err)
			// 继续处理其他 Sentinel
		} else {
			logger.Info("Sentinel REMOVE 成功", "pod", pod.Name, "result", result)
			successCount++
		}

		sentinelClient.Close()
	}

	if successCount == 0 {
		return fmt.Errorf("所有 Sentinel 节点都移除监控失败")
	}

	logger.Info("完成 Sentinel 监控注销", "name", redis.Name, "success", successCount, "total", len(podList.Items))
	return nil
}

// containsString 检查字符串切片是否包含指定字符串
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

// removeString 从字符串切片中移除指定字符串
func removeString(slice []string, s string) []string {
	var result []string
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

// reconcileSentinel 调和 Sentinel 相关资源（StatefulSet、ConfigMap、Service）
func (r *RedisClusterReconciler) reconcileSentinel(ctx context.Context, redis *dbv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// 1. 创建或更新 Sentinel ConfigMap
	sentinelCMName := redis.Name + "-sentinel-config"
	if err := r.reconcileSentinelConfigMap(ctx, redis, sentinelCMName); err != nil {
		return fmt.Errorf("调和 Sentinel ConfigMap 失败: %w", err)
	}

	// 2. 创建或更新 Sentinel Service
	sentinelSvcName := redis.Name + "-sentinel"
	sentinelSvc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: sentinelSvcName, Namespace: redis.Namespace}, sentinelSvc)
	if err != nil && errors.IsNotFound(err) {
		newSentinelSvc := r.constructSentinelService(redis, sentinelSvcName)
		logger.Info("创建 Sentinel Service", "name", sentinelSvcName)
		if err := r.Create(ctx, newSentinelSvc); err != nil {
			return err
		}
	}

	// 3. 创建或更新 Sentinel StatefulSet
	sentinelStsName := redis.Name + "-sentinel"
	sentinelSts := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: sentinelStsName, Namespace: redis.Namespace}, sentinelSts)
	if err != nil && errors.IsNotFound(err) {
		newSentinelSts := r.constructSentinelStatefulSet(redis, sentinelStsName, sentinelCMName)
		logger.Info("创建 Sentinel StatefulSet", "name", sentinelStsName)
		if err := r.Create(ctx, newSentinelSts); err != nil {
			return err
		}
	} else if err == nil {
		// 检查并更新副本数
		sentinelReplicas := redis.Spec.SentinelConfig.Replicas
		if sentinelReplicas == 0 {
			sentinelReplicas = 3 // 默认3个副本
		}
		if *sentinelSts.Spec.Replicas != sentinelReplicas {
			sentinelSts.Spec.Replicas = &sentinelReplicas
			if err := r.Update(ctx, sentinelSts); err != nil {
				return err
			}
		}
	}

	return nil
}

// reconcileSentinelConfigMap 创建或更新 Sentinel ConfigMap
func (r *RedisClusterReconciler) reconcileSentinelConfigMap(ctx context.Context, redis *dbv1.RedisCluster, cmName string) error {
	logger := log.FromContext(ctx)

	// 构建 sentinel.conf 内容
	sentinelConf := r.generateSentinelConfig(redis)

	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: redis.Namespace}, cm)
	if err != nil && errors.IsNotFound(err) {
		// ConfigMap 不存在，创建新的
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: redis.Namespace,
			},
			Data: map[string]string{
				"sentinel.conf": sentinelConf,
			},
		}
		ctrl.SetControllerReference(redis, newCM, r.Scheme)
		logger.Info("创建 Sentinel ConfigMap", "name", cmName)
		return r.Create(ctx, newCM)
	} else if err == nil {
		// ConfigMap 已存在，检查是否需要更新
		if cm.Data["sentinel.conf"] != sentinelConf {
			cm.Data["sentinel.conf"] = sentinelConf
			logger.Info("更新 Sentinel ConfigMap", "name", cmName)
			return r.Update(ctx, cm)
		}
	}

	return err
}

// generateSentinelConfig 生成 Sentinel 配置文件内容
func (r *RedisClusterReconciler) generateSentinelConfig(redis *dbv1.RedisCluster) string {
	// 获取配置参数
	config := redis.Spec.SentinelConfig
	if config == nil {
		config = &dbv1.SentinelConfig{}
	}

	quorum := config.Quorum
	if quorum == 0 {
		quorum = 2
	}

	downAfter := config.DownAfterMilliseconds
	if downAfter == 0 {
		downAfter = 5000
	}

	failoverTimeout := config.FailoverTimeoutMilliseconds
	if failoverTimeout == 0 {
		failoverTimeout = 10000
	}

	// Master 的 DNS 地址（第一个 Redis Pod）
	masterDNS := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", redis.Name, redis.Name, redis.Namespace)

	// 生成配置文件内容
	conf := fmt.Sprintf(`# Generated by Redis Operator
port %d
dir /tmp
daemonize no
pidfile /tmp/sentinel.pid
logfile ""

monitor mymaster %s 6379 %d
down-after-milliseconds mymaster %d
failover-timeout mymaster %d
parallel-syncs mymaster 1
`,
		config.Port,
		masterDNS,
		quorum,
		downAfter,
		failoverTimeout,
	)

	// 如果 Redis 设置了密码，添加认证配置
	if redis.Spec.Password != "" {
		conf += fmt.Sprintf("auth-pass mymaster %s\n", redis.Spec.Password)
	}

	return conf
}

// constructSentinelService 创建 Sentinel Service
func (r *RedisClusterReconciler) constructSentinelService(redis *dbv1.RedisCluster, name string) *corev1.Service {
	sentinelPort := int32(26379)
	if redis.Spec.SentinelConfig != nil && redis.Spec.SentinelConfig.Port != 0 {
		sentinelPort = redis.Spec.SentinelConfig.Port
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: redis.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: "sentinel",
					Port: sentinelPort,
				},
			},
			Selector: map[string]string{
				"app":        redis.Name,
				"component": "sentinel",
			},
			ClusterIP: "None", // Headless Service
		},
	}
	ctrl.SetControllerReference(redis, svc, r.Scheme)
	return svc
}

// constructSentinelStatefulSet 创建 Sentinel StatefulSet
func (r *RedisClusterReconciler) constructSentinelStatefulSet(redis *dbv1.RedisCluster, name, configMapName string) *appsv1.StatefulSet {
	sentinelPort := int32(26379)
	if redis.Spec.SentinelConfig != nil && redis.Spec.SentinelConfig.Port != 0 {
		sentinelPort = redis.Spec.SentinelConfig.Port
	}

	replicas := redis.Spec.SentinelConfig.Replicas
	if replicas == 0 {
		replicas = 3 // 默认3个副本
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: redis.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":        redis.Name,
					"component": "sentinel",
				},
			},
			ServiceName: name,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":        redis.Name,
						"component": "sentinel",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "sentinel",
							Image: redis.Spec.Image,
							Ports: []corev1.ContainerPort{{ContainerPort: sentinelPort}},
							Command: []string{
								"redis-sentinel",
								"/etc/sentinel/sentinel.conf",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: "/etc/sentinel",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapName,
									},
								},
							},
						},
					},
				},
			},
		},
	}
	ctrl.SetControllerReference(redis, sts, r.Scheme)
	return sts
}

// checkFailoverAndHealth 检测故障转移和节点健康状态
func (r *RedisClusterReconciler) checkFailoverAndHealth(ctx context.Context, redis *dbv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// 1. 获取当前 Master 地址
	currentMaster, err := r.getCurrentMasterFromSentinel(ctx, redis)
	if err != nil {
		logger.Error(err, "获取当前 Master 失败")
		return err
	}

	// 2. 检测是否发生了故障转移
	expectedMaster := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", redis.Name, redis.Name, redis.Namespace)
	if currentMaster != expectedMaster {
		logger.Info("检测到故障转移", "old-master", expectedMaster, "new-master", currentMaster)
		// 更新主从配置
		if err := r.reconfigureAfterFailover(ctx, redis, currentMaster); err != nil {
			logger.Error(err, "故障转移后重新配置失败")
			return err
		}
	}

	// 3. 检查所有节点健康状态
	if err := r.checkNodesHealth(ctx, redis); err != nil {
		logger.Error(err, "节点健康检查失败")
		return err
	}

	return nil
}

// getCurrentMasterFromSentinel 从 Sentinel 获取当前 Master 地址
func (r *RedisClusterReconciler) getCurrentMasterFromSentinel(ctx context.Context, redis *dbv1.RedisCluster) (string, error) {
	// 获取第一个 Sentinel Pod
	sentinelPodName := fmt.Sprintf("%s-sentinel-0", redis.Name)
	sentinelDNS := fmt.Sprintf("%s.%s-sentinel.%s.svc.cluster.local", sentinelPodName, redis.Name, redis.Namespace)

	sentinelPort := int32(26379)
	if redis.Spec.SentinelConfig != nil && redis.Spec.SentinelConfig.Port != 0 {
		sentinelPort = redis.Spec.SentinelConfig.Port
	}

	// 连接到 Sentinel
	sentinelClient := goRedis.NewSentinelClient(&goRedis.Options{
		Addr:        fmt.Sprintf("%s:%d", sentinelDNS, sentinelPort),
		Password:    redis.Spec.Password,
		DialTimeout: 2 * time.Second,
		MaxRetries:  3,
	})
	defer sentinelClient.Close()

	// 获取 Master 地址（返回 []string，格式为 [host, port]）
	addrs, err := sentinelClient.GetMasterAddrByName(ctx, "mymaster").Result()
	if err != nil {
		return "", fmt.Errorf("从 Sentinel 获取 Master 地址失败: %w", err)
	}

	if len(addrs) < 2 {
		return "", fmt.Errorf("无效的 Master 地址格式: %v", addrs)
	}

	// 返回 host 部分
	return addrs[0], nil
}

// reconfigureAfterFailover 故障转移后重新配置主从关系
func (r *RedisClusterReconciler) reconfigureAfterFailover(ctx context.Context, redis *dbv1.RedisCluster, newMaster string) error {
	logger := log.FromContext(ctx)

	// 1. 将旧 Master（redis-0）配置为新 Master 的 Slave
	oldMasterPod := fmt.Sprintf("%s-0", redis.Name)
	oldMasterDNS := fmt.Sprintf("%s.%s.%s.svc.cluster.local", oldMasterPod, redis.Name, redis.Namespace)

	rdb := goRedis.NewClient(&goRedis.Options{
		Addr:        oldMasterDNS + ":6379",
		Password:    redis.Spec.Password,
		DialTimeout: 2 * time.Second,
	})
	defer rdb.Close()

	if err := rdb.SlaveOf(ctx, newMaster, "6379").Err(); err != nil {
		logger.Error(err, "配置旧 Master 为 Slave 失败", "old-master", oldMasterDNS)
		return err
	}

	logger.Info("成功配置旧 Master 为新 Master 的 Slave", "old-master", oldMasterDNS, "new-master", newMaster)
	return nil
}

// checkNodesHealth 检查所有 Redis 节点的健康状态
func (r *RedisClusterReconciler) checkNodesHealth(ctx context.Context, redis *dbv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	unhealthyCount := 0

	for i := 0; i < int(redis.Spec.Replicas); i++ {
		podName := fmt.Sprintf("%s-%d", redis.Name, i)
		podDNS := fmt.Sprintf("%s.%s.%s.svc.cluster.local", podName, redis.Name, redis.Namespace)

		rdb := goRedis.NewClient(&goRedis.Options{
			Addr:        podDNS + ":6379",
			Password:    redis.Spec.Password,
			DialTimeout: 2 * time.Second,
			MaxRetries:  1, // 快速失败
		})

		// 执行 PING 命令检查健康
		if err := rdb.Ping(ctx).Err(); err != nil {
			logger.Info("节点不健康", "pod", podName, "error", err)
			unhealthyCount++
		}

		rdb.Close()
	}

	if unhealthyCount > 0 {
		logger.Info("健康检查完成", "unhealthy", unhealthyCount, "total", redis.Spec.Replicas)
	}

	return nil
}

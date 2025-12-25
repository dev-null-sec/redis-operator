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
	"github.com/redis/go-redis/v9"
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
	} else if err == nil {
		// 检查并更新副本数
		if *sts.Spec.Replicas != redis.Spec.Replicas {
			sts.Spec.Replicas = &redis.Spec.Replicas
			if err := r.Update(ctx, sts); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

// 4. 集群拓扑配置
	// 当启用哨兵模式且所有节点就绪时执行
	if redis.Spec.EnableSentinel && sts.Status.ReadyReplicas == redis.Spec.Replicas {
		r.reconcileClusterTopology(ctx, redis)
	}

	// 5. 更新 RedisCluster 状态
	redis.Status.ReadyReplicas = sts.Status.ReadyReplicas
	redis.Status.State = "Running"
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
		Complete(r)
}

// reconcileClusterTopology 配置 Redis 主从复制
func (r *RedisClusterReconciler) reconcileClusterTopology(ctx context.Context, redisCR *dbv1.RedisCluster) {
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

		// 创建 Redis 客户端连接
		rdb := redis.NewClient(&redis.Options{
			Addr:        podDNS + ":6379",
			Password:    "",
			DialTimeout: 2 * time.Second,
		})
		defer rdb.Close()

		if i == 0 {
			// 配置为 Master 节点
			if err := rdb.SlaveOf(ctx, "NO", "ONE").Err(); err != nil {
				logger.Error(err, "设置 Master 失败", "pod", podName)
			} else {
				logger.Info("成功设置 Master 节点", "pod", podName)
			}
		} else {
			// 配置为 Slave 节点，指向 Master
			if err := rdb.SlaveOf(ctx, masterDNS, "6379").Err(); err != nil {
				logger.Error(err, "设置 Slave 失败", "pod", podName, "master", masterDNS)
			} else {
				logger.Info("成功设置 Slave 节点", "pod", podName)
			}
		}
	}
}
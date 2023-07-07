# Karmada简单控制器实现

目标:实现一个自定义Controller直接渲染并创建 Work 对象，让 Execution Controller 和 Karmada Agent 去 Reconcile，从而无需创建Karmada PropagationPolicy



## 背景知识

### Controller（控制器）

Kubernetes 控制器会监听资源的 `创建/更新/删除` 事件，并触发 `Reconcile` 调谐函数作为响应，整个调整过程被称作 `“Reconcile Loop”（调谐循环）` 或者 `“Sync Loop”（同步循环）`。Reconcile 是一个使用资源对象的命名空间和资源对象名称来调用的函数，使得资源对象的实际状态与 资源清单中定义的状态保持一致。调用完成后，Reconcile 会将资源对象的状态更新为当前实际状态。我们可以用下面的一段伪代码来表示这个过程：

`for` `{`

  `desired :``= getDesiredState()  // 期望的状态`

  `current :``= getCurrentState()  // 当前实际状态`

  `if current == desired` `{`  `// 如果状态一致则什么都不做`

    `// nothing to do`

  `}` `else` `{`  `// 如果状态不一致则调整编排，到一致为止`

    `// change current to desired status`

  `}`

`}`

这个编排模型就是 Kubernetes 项目中的一个通用编排模式，即：`控制循环（control loop）`。

### kube-controller-manager

kube-controller-manager是Kubernetes的一个核心组件，负责在Kubernetes集群中运行各种基本的控制器。这些控制器处理了很多核心的后台任务，以确保集群的正常运行和健康。

以下是kube-controller-manager运行的一些主要控制器：

1. Node Controller：当节点变为不可用时，Node Controller会负责做出响应。

2. Job Controller：负责执行批处理作业。

3. Endpoints Controller：负责链接Services和Pods。

4. ServiceAccount Controller：负责为新的namespace创建默认的ServiceAccount。

5. ReplicaSet Controller：确保每个ReplicaSet中的Pod数量总是正确的。

6. Namespace Controller：负责处理新创建的和已经删除的namespaces。

kube-controller-manager实现了这些基于资源的控制循环，它会定期从API server获取资源的状态，然后根据资源的当前状态和期望状态，进行相应的操作以驱动当前状态向期望状态转变。

kube-controller-manager通常会和kube-apiserver、kube-scheduler以及kubelet等其他组件一起运行，以形成Kubernetes控制平面。同时，kube-controller-manager可以使用高可用配置运行，通过leader election机制保证任何时刻只有一个active leader在执行操作，其他的实例处于standby状态，这样可以提高系统的稳定性和可用性。

### Leader Election

领导选举是一个分布式系统中常见的协议，用于在一组节点（在这种情况下，为kube-controller-manager的实例）中选择一个leader，该leader将负责处理所有工作，而其他节点则进入待命模式。

领导选举的主要目的是提高系统的可用性和稳定性。如果没有领导选举，那么在高可用配置下运行的kube-controller-manager的所有实例都会尝试独立地执行同样的任务，这可能会导致冲突和不一致。通过选举出一个leader，系统可以确保只有一个实例会处理工作，从而避免这种问题。

在Kubernetes中，领导选举是通过使用一个特定的锁对象（如Lease，Endpoint或ConfigMap）实现的。kube-controller-manager的一个实例会尝试获取这个锁，如果成功，它就会成为leader。只有leader才会执行控制循环和其他任务。其他的kube-controller-manager实例会一直监视这个锁，如果leader失效（比如崩溃或者无法与API server通信），它们会看到锁已经被释放，然后开始新一轮的领导选举。

选举过程大致如下：

1. 每个kube-controller-manager实例都会尝试获取锁。这通常涉及到在锁定对象上设置其自己的标识信息（例如，它的名称和当前时间戳）。

2. 如果实例成功地获取了锁（也就是成功地更新了锁对象），那么它就成为领导者。

3. 一旦成为领导者，该实例就会定期更新锁对象以维持其领导地位。这被称为锁的"续约"。

4. 如果领导者实例崩溃或无法进行续约（例如，它无法与API server通信），那么其他的实例会注意到锁对象没有被更新，于是它们开始新一轮的领导者选举。

5. 新的领导者选举成功后，新的领导者就会接管，开始执行任务。

### karmada-controller-manager

karmada-controller-manager运行各种控制器。这些控制器Watch Karmada对象，然后与集群的API Server通信，以创建常规的Kubernetes资源。

相关配置参数：

--leader-elect 在执行主循环之前启动一个领导者选举客户端并获得领导权。当运行复制的组件以获得高可用性时，请启用此功能。(默认为true)  
--leader-elect-le-duration 持续时间 非领导者候选人在观察到领导权更新后，在试图获得一个已被领导但未被更新的领导者槽的领导权之前所要等待的时间。这实际上是一个领导者在被其他候选人取代之前可以被阻止的最长时间。这只适用于领袖选举被启用的情况。(默认为15s)  
--leader-elect-renew-deadline duration 代理主站在停止领导前尝试更新一个领导位置的间隔时间。这必须小于或等于租赁期限。这只适用于启用了领导选举的情况。(默认为10s)  
--leader-elect-resource-namespace string 在领导者选举期间用于锁定的资源对象的命名空间。(默认为 "karmada-system")  
--leader-elect-retry-period duration 客户端在尝试获取和更新领导权之间应该等待的时间。这只适用于领袖选举被启用的情况。(默认为2s)

# Лаба 5

## НЕЙРОНКА: Что уже переиспользовано

- `helm/shop` — чистая копия чарта `shop` из Лабы 3. В ней уже есть `api`,
  `worker`, PostgreSQL через CloudNativePG, базовый dashboard,
  `ServiceMonitor` и `PrometheusRule`.
- Образы `docker.io/qerenny/lab3-api:0.2.0` и
  `docker.io/qerenny/lab3-worker:0.1.0` используются напрямую. Исходники
  сервисов остаются в `lab3`: программная часть в этой лабораторной не
  меняется.
- `load/order.js` перенесён из Лабы 4 как исходный генератор заказов. Его
  пороги пока относятся к SLA Лабы 4; для доказательства нулевого числа
  ошибок их предстоит адаптировать.

Чарт скопирован, а не подключён как зависимость, потому что результатом Лабы 5
должен стать самостоятельно изменяемый и сдаваемый чарт. При этом уже
закрытые лабораторные остаются неизменными.

Не переиспользуется `lab4/kind-config.yaml`: он запускает стандартный
`kindnet`, который не применяет NetworkPolicy. Конфигурация кластера зависит
от выбора Cilium или Calico и является частью работы.

Базовые `ServiceMonitor`, `PrometheusRule` и dashboard подтверждают метрики
приложения, но сами по себе не закрывают Часть 7: сетевые метрики и правила
будут зависеть от выбранного CNI.

# Часть 0 -- Выбираем CNI

Для сети взял Cilium 1.20.2. Calico тоже умеет применять NetworkPolicy, но
здесь дальше понадобится Hubble: через него можно увидеть потоки и причину
policy drop.

Стандартный `kindnet` отключаем сразу иначе API server спокойно примет
NetworkPolicy, но CNI применять её не будет и эксперимент получится немного
бессмысленным.

```yaml
# lab5/kind-config.yaml
networking:
  disableDefaultCNI: true
  podSubnet: 10.50.0.0/16
  serviceSubnet: 10.51.0.0/16
```

Поднимаем две worker-ноды, чтобы дальше действительно погонять пакет между
нодами. 
```bash
kind create cluster \
  --name lab5 \
  --config lab5/kind-config.yaml
```

До установки CNI ноды были `NotReady`, что тут как раз нормально: kubelet уже
жив, но подключить pod к сети пока некому. Ставим Cilium с VXLAN и Hubble:

```bash
helm upgrade --install cilium \
  oci://quay.io/cilium/charts/cilium \
  --version 1.20.2 \
  --namespace kube-system \
  --values lab5/cilium-values.yaml \
  --wait \
  --timeout 10m
```

Полную eBPF-замену kube-proxy пока не включал:

```yaml
kubeProxyReplacement: false
```

Так Cilium занимается сетью подов, политиками и наблюдаемостью, но обычный
`kube-proxy` остаётся в кластере — его работу с Service и NodePort ещё будем
разбирать отдельно как вроде сказано дальше в лабе.

Проверяем, что все сетевые компоненты доехали:

```bash
qerenny@containers:~/containers-labs$ kubectl -n kube-system rollout status daemonset/cilium
daemon set "cilium" successfully rolled out

qerenny@containers:~/containers-labs$ kubectl -n kube-system rollout status deployment/cilium-operator
deployment "cilium-operator" successfully rolled out

qerenny@containers:~/containers-labs$ kubectl -n kube-system rollout status deployment/hubble-relay
deployment "hubble-relay" successfully rolled out

qerenny@containers:~/containers-labs$ kubectl -n kube-system get daemonset kube-proxy
NAME         DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE
kube-proxy   3         3         3       3            3
```

В итоге Cilium agent и kube-proxy стоят на каждой ноде, Hubble Relay готов,
все три ноды перешли в `Ready`. Можно наконец посмотреть, что Cilium там
насоздавал.

# Часть 1 -- Лезем в сеть пода

Первым делом смотрим, какие подсети Kubernetes раздал нодам:

```bash
qerenny@containers:~/containers-labs$ kubectl get nodes \
  -o custom-columns='NODE:.metadata.name,NODE_IP:.status.addresses[?(@.type=="InternalIP")].address,POD_CIDR:.spec.podCIDR'
NODE                 NODE_IP      POD_CIDR
lab5-control-plane   172.18.0.6   10.50.0.0/24
lab5-worker          172.18.0.5   10.50.1.0/24
lab5-worker2         172.18.0.7   10.50.3.0/24
```

`172.18.0.x` — адреса самих kind-нод в Docker-сети. `10.50.x.x` — уже сеть
подов. Kubernetes выдал каждой ноде свой PodCIDR `/24`, а Cilium IPAM раздаёт
подам конкретные адреса из подсети их ноды. 


В образе API, конечно же, distroless: ни shell, ни `ip`, ничего полезного для
наших раскопок. Пробуем подселить к нему `netshoot` в тот же network
namespace:

```bash
qerenny@containers:~/containers-labs$ kubectl debug -n shop \
  -it pod/shop-api-5f9ccdd865-7v9vz \
  --image=nicolaka/netshoot \
  --target=api \
  --profile=general

Warning: container debugger-jz2x9: container has runAsNonRoot and image has
non-numeric user (root), cannot verify user is non-root
```

С первого раза нас  послали: у Pod стоит `runAsNonRoot`, а у
`netshoot` пользователь записан строкой `root`.  Делаем временный профиль с явным UID и оставляем только
`NET_RAW`, который нужен для `ping`:

```yaml
# /tmp/lab5-debug-profile.yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  allowPrivilegeEscalation: false
  capabilities:
    add:
      - NET_RAW
```

```bash
API_POD_NAME=$(kubectl -n shop get pods \
  -l app.kubernetes.io/name=api \
  --field-selector spec.nodeName=lab5-worker2 \
  -o jsonpath='{.items[0].metadata.name}')

kubectl debug \
  -n shop \
  -it "pod/$API_POD_NAME" \
  --image=nicolaka/netshoot \
  --target=api \
  --profile=general \
  --custom=/tmp/lab5-debug-profile.yaml \
  -- sh
```

Смотрим адрес, интерфейс и таблицу маршрутов изнутри
network namespace пода:

```bash
/root $ ip -br address show eth0
eth0@if21  UP  10.50.3.191/32  fe80::e4f9:86ff:fe27:86e9/64

/root $ ip -o link show eth0
20: eth0@if21: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...

/root $ ip route
default via 10.50.3.161 dev eth0 mtu 1450
10.50.3.161 dev eth0 scope link
```

У пода не целая локальная подсеть, а один адрес `/32`. Всё остальное он
отправляет через gateway Cilium `10.50.3.161`, поэтому пакет обязательно
выходит через `eth0` в datapath ноды. На route MTU уже `1450`: ещё примерно
50 байт понадобятся внешним заголовкам.

Теперь ищем второй конец `eth0@if21` на ноде. Сначала я честно попытался
скормить `ip` всё отображаемое имя:

```bash
qerenny@containers:~/containers-labs$ docker exec lab5-worker2 \
  ip -d link show lxc0f409f1cd7dc@if20
Device "lxc0f409f1cd7dc@if20" does not exist.
```

`@if20` оказался не частью имени, а индексом peer-интерфейса........

```bash
qerenny@containers:~/containers-labs$ docker exec lab5-worker2 \
  ip -d link show lxc0f409f1cd7dc
21: lxc0f409f1cd7dc@if20: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...
    link-netns cni-bde370ee-a294-56f6-13b3-8b091a0c605d
    veth ...
```

Получилась зеркальная пара.
То есть `eth0` пода и `lxc...` ноды — два конца одного veth в разных network
namespace. Именно через него пакет выходит из пода на ноду.

Проверяем связь напрямую с API-подом на другой ноде, без DNS, Service и
ClusterIP:

```bash
/root $ ping -c 3 10.50.1.38
PING 10.50.1.38 (10.50.1.38) 56(84) bytes of data.
64 bytes from 10.50.1.38: icmp_seq=1 ttl=63 time=0.519 ms
64 bytes from 10.50.1.38: icmp_seq=2 ttl=63 time=0.805 ms
64 bytes from 10.50.1.38: icmp_seq=3 ttl=63 time=0.532 ms

3 packets transmitted, 3 received, 0% packet loss
```

Для пода это обычный пакет `10.50.3.191 -> 10.50.1.38`. Ноды при этом знают
друг друга по адресам `172.18.0.7` и `172.18.0.5`, а Cilium перевозит pod
packet между ними внутри VXLAN.

```bash
qerenny@containers:~/containers-labs$ docker exec lab5-worker2 \
  ip -d link show cilium_vxlan
5: cilium_vxlan: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...
    vxlan id 0 srcport 0 0 dstport 8472 ttl auto ageing 300 external nolearning ...
```


Напоследок смотрим endpoint Cilium для нашего IP:

```bash
CILIUM_POD_NAME=$(kubectl -n kube-system get pods \
  -l k8s-app=cilium \
  --field-selector spec.nodeName=lab5-worker2 \
  -o jsonpath='{.items[0].metadata.name}')

qerenny@containers:~/containers-labs$ kubectl -n kube-system \
  exec "$CILIUM_POD_NAME" -- cilium-dbg endpoint list | grep '10.50.3.191'
41  Disabled  Disabled  64467  gen:io.cilium.k8s.named-ports=http.TCP.8080  10.50.3.191  ready
```

Оба `Disabled` означают, что ingress- и egress-policy enforcement пока не
включены: ни одной NetworkPolicy у нас ещё нет. Потом сравним этот вывод с
default-deny.

В сухом остатке пакет прошёл такой путь: из eth0 первого Pod он попал в парный lxc/veth-интерфейс на ноде lab5-worker2, затем прошёл через eBPF datapath Cilium. После этого пакет был инкапсулирован в VXLAN и передан по Docker-сети kind на ноду lab5-worker. Там он снова прошёл через eBPF datapath Cilium, затем через veth второго Pod и в итоге попал в его eth0.
## Часть 2 — непрерывность через Service

TODO: NodePort, сценарий нагрузки и хаоса, график, EndpointSlice, реализация
Service в выбранном CNI или kube-proxy.

## Часть 3 — целевая матрица доступа

TODO: матрица до написания политик, включая DNS и egress.

## Часть 4 — zero-trust

TODO: принципы реализации матрицы в Helm-чарте.

## Часть 5 — фактическая матрица доступа

TODO: ожидаемый и фактический результат для каждой пары.

## Часть 6 — диагностика скрытой поломки

TODO: симптом, наблюдение сетевого пути, найденная причина и подтверждение
исправления.

## Часть 7 — мониторинг

TODO: сетевые метрики, dashboard, три алерта и доказательство хотя бы одного
срабатывания.

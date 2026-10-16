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

Сначала выставил API наружу через `NodePort`. Внутри кластера у сервиса всё
так же остаётся `ClusterIP`, просто в дополнение к нему kube-proxy открывает
один и тот же порт на каждой ноде:

```yaml
# lab5/helm/shop/values.yaml
api:
  service:
    type: NodePort
    port: 8080
    nodePort: 30080
```

В kind порт `30080` control-plane ноды проброшен на `18080` хоста. Проверяю
внешнюю точку входа:

```bash
qerenny@containers:~/containers-labs$ curl -v http://127.0.0.1:18080/health
> GET /health HTTP/1.1
< HTTP/1.1 200 OK

ok

qerenny@containers:~/containers-labs$ kubectl -n shop get service shop-api
NAME       TYPE       CLUSTER-IP     EXTERNAL-IP   PORT(S)
shop-api   NodePort   10.51.137.78   <none>        8080:30080/TCP
```

`NodePort` подходит для этой лабы, потому что внешний балансировщик мне в
локальном kind не нужен. `LoadBalancer` попросил бы отдельный контроллер
создать внешний балансировщик, которого тут нет. У headless-сервиса вообще
не было бы виртуального IP. Здесь нужен обратный эффект — один стабильный адрес,
за которым поды могут постоянно меняться.

Изнутри кластера клиент ходит не через NodePort, а по имени `shop-api`:

```bash
qerenny@containers:~/containers-labs$ kubectl run http-check \
  --namespace shop \
  --image=curlimages/curl \
  --restart=Never \
  --rm -it -- curl -v http://shop-api:8080/health

* Host shop-api:8080 was resolved.
* IPv4: 10.51.137.78
* Established connection to shop-api (10.51.137.78 port 8080)
< HTTP/1.1 200 OK

ok
```

То есть CoreDNS превратил короткое имя в
`shop-api.shop.svc.cluster.local` и вернул стабильный ClusterIP
`10.51.137.78`. Адреса подов DNS здесь вообще не интересуют.

### Нагрузка и непрерывный хаос

В `load/order.js` поставил  пороги: не допускается ни одного
неуспешного HTTP-запроса и ни одной проваленной проверки. Pod с k6 делает
20 `POST /order` в секунду в течение четырёх минут:

```javascript
'http_req_failed{endpoint:order}': ['rate==0'],
'checks{endpoint:order}': ['rate==1'],
```

Параллельно `scripts/chaos-pods.sh` каждые пять секунд выбирает случайный
`api` или `worker` и удаляет его с `--wait=false`.

Первый запуск с обычным завершением Pod оказался неидеальным:

```text
✗ order created
 ↳  99% — ✓ 4789 / ✗ 12

checks.................: 99.75% 4789 out of 4801
http_req_failed........: 0.24%  12 out of 4801
http_req_duration......: p(95)=21.21ms
```

При этом в логе хаоса у API всегда оставалось минимум два ready endpoint.
Добавлять ещё реплики поэтому бессмысленно.


Service выбирает backend только для нового соединения. После DNAT запись
 продолжает отправлять пакеты этого TCP-соединения в тот же Pod.
Если kubelet уже послал процессу `SIGTERM`, то отправленый пакет никто не отменит. Отсюда и редкие обрывы на фоне
нормального p95.

Чтобы не винить приложение во всех бедах, отдельно прогнал те же четыре минуты вообще
без chaos:

```text
checks.................: 100.00% 4801 out of 4801
http_req_failed........: 0.00%   0 out of 4801
http_req_duration......: p(95)=17.9ms
```

Baseline чистый, поэтому исправил именно завершение API. Образ distroless,
так что `exec: sleep 5` внутри него не сработал бы — там нет shell и утилиты
`sleep`. Вместо этого использовал lifecycle sleep-handler, который выполняет
сам kubelet:

![Графики контрольного запуска k6 без chaos](pics/k6-baseline.png)

На контрольном графике нагрузка держится около 20 запросов в секунду, а
`Request Failed Rate` всё время остаётся на нуле.

```yaml
terminationGracePeriodSeconds: 20

lifecycle:
  preStop:
    sleep:
      seconds: 5
```

При удалении endpoint становится terminating и перестаёт выбираться для
новых соединений, но сам API ещё пять секунд продолжает обслуживать старые.
После `preStop` kubelet отправляет `SIGTERM`, а Go-сервис уже сам вызывает
`http.Server.Shutdown()` с timeout 10 секунд. Поэтому общий grace period
сделан 20 секунд: пять на распространение состояния сети, десять на
shutdown и ещё небольшой запас.

Во втором запуске за четыре минуты скрипт удалил 38 подов: 26 API и 12
worker. Готовых API и endpoint при этом было не меньше двух. Полный журнал:
[`reports/chaos-graceful.log`](reports/chaos-graceful.log).

```text
Starting pod chaos in namespace=shop for 240s, interval=5s
2026-10-15T22:20:59+00:00 deleting pod=shop-api-59bf89865c-4xxqs
component=api ip=10.50.3.168 ready=api:3,worker:2
api_endpoints=[10.50.1.184 10.50.3.168 10.50.1.219]
...
Pod chaos finished
```

И на этот раз k6 не потерял ни одного запроса:

```text
✓ order created

checks.........................: 100.00% 4800 out of 4800
http_req_failed................: 0.00%   0 out of 4800
http_req_duration..............: p(95)=24.47ms
http_reqs......................: 4800    19.999908/s
```

Графики всего четырёхминутного интервала лежат в
[`reports/k6-chaos-graceful.html`](reports/k6-chaos-graceful.html). На них
видно постоянные 20 запросов в секунду и нулевой `http_req_failed` всё время,
пока в соседнем логе непрерывно меняются имена и IP подов.

### Кто что делал в сетевом стеке

- CoreDNS возвращал клиенту стабильный ClusterIP.
- Service задавал виртуальную точку входа и selector для API.
- Контроллер EndpointSlice следил за подходящими Pod и обновлял их IP и
  состояния `ready`/`terminating`.
- kube-proxy смотрел на Service и EndpointSlice и программировал на нодах
  правила DNAT.
- conntrack запоминал выбранный backend на время существования соединения.
- Deployment видел удаление Pod и создавал замену, а Cilium выдавал ей новый
  IP и подключал к pod-сети.

Проверил фактический режим kube-proxy:

```bash
qerenny@containers:~/containers-labs$ kubectl -n kube-system get configmap kube-proxy \
  -o jsonpath='{.data.config\.conf}' | grep -E '^mode:|^[[:space:]]+mode:'
mode: iptables

qerenny@containers:~/containers-labs$ kubectl -n kube-system logs daemonset/kube-proxy \
  --tail=200 | grep -Ei 'proxier|iptables|ipvs'
I1013 19:46:23.333424 1 server_linux.go:137] "Using iptables Proxier"
```

В моём масштабе разницу между iptables и IPVS по производительности увидеть
практически негде: сервисов мало, endpoint у API всего три. Она становится
заметнее в больших кластерах с тысячами endpoint и частыми
изменениями. В iptables backend выбирается цепочками правил по очереди, а IPVS хранит
виртуальные сервисы и backend в собственных таблицах и имеет отдельные
алгоритмы планирования.
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

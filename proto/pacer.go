// Пейсер исходящего видео-RTP — размазывает пакеты кадра по времени вместо залпа.
//
// pion, в отличие от libwebrtc, не имеет paced sender'а: WriteSample отдаёт ВСЕ
// RTP-пакеты кадра подряд в линию. P-кадр — это 5-6 пакетов, ключевой кадр 100КБ —
// 80+ пакетов одним залпом на линейной скорости. Шейперы/полисеры провайдеров
// (особенно с DPI) режут такие UDP-залпы по хвосту: средний битрейт 1-3 Мбит на
// 80-Мбит канале, а потери есть — строго на границах кадров. Дальше два сценария,
// оба наблюдались в логах: мелкая потеря → NACK-ретрансмит (+RTT ≈ 300мс фриз у
// зрителя, в логах хоста тишина), крупная (хвост IDR) → PLI → форс-IDR → новый залп.
//
// Решение то же, что в libwebrtc: очередь + leaky bucket на ~2.5× целевого битрейта
// (запас, чтобы кейфрейм уходил быстро, но не мгновенно). Регистрируется ПЕРВЫМ в
// interceptor.Registry — т.е. самым внутренним слоем: NACK-ретрансмиты, добавленные
// внешними интерсепторами, тоже проходят через пейсер.
//
// KATANA_NO_PACER=1 выключает (для A/B-сравнения на живом линке).
package main

import (
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// pacerTargetBps — целевая скорость слива (бит/с) = битрейт энкодера × 2.5.
// Общая для всех зрителей (треки общие, битрейт один). Обновляется при смене
// битрейта (настройки/AIMD/PLI-шторм) через setPacerBitrateKbps.
var pacerTargetBps atomic.Int64

func init() { pacerTargetBps.Store(3000 * 1000 * 5 / 2) } // дефолт до первой настройки

// setPacerBitrateKbps подстраивает скорость пейсера под текущий битрейт энкодера.
func setPacerBitrateKbps(kbps int) {
	if kbps > 0 {
		pacerTargetBps.Store(int64(kbps) * 1000 * 5 / 2)
	}
}

type pacerFactory struct{}

func (pacerFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &pacerInterceptor{writers: map[uint32]*pacedWriter{}}, nil
}

type pacerInterceptor struct {
	interceptor.NoOp
	mu      sync.Mutex
	writers map[uint32]*pacedWriter // по SSRC — для Unbind/Close
}

func (p *pacerInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	// Только видео: аудио-пакеты мелкие и равномерные, им пейсинг не нужен,
	// а лишняя задержка вредна.
	if os.Getenv("KATANA_NO_PACER") == "1" || !strings.HasPrefix(strings.ToLower(info.MimeType), "video/") {
		return writer
	}
	w := newPacedWriter(writer)
	p.mu.Lock()
	p.writers[info.SSRC] = w
	p.mu.Unlock()
	log.Printf("pacer: видео-RTP пейсинг ON (%s ssrc=%d, ×2.5 битрейта)", info.MimeType, info.SSRC)
	return w
}

func (p *pacerInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	p.mu.Lock()
	w := p.writers[info.SSRC]
	delete(p.writers, info.SSRC)
	p.mu.Unlock()
	if w != nil {
		w.stop()
	}
}

func (p *pacerInterceptor) Close() error {
	p.mu.Lock()
	ws := p.writers
	p.writers = map[uint32]*pacedWriter{}
	p.mu.Unlock()
	for _, w := range ws {
		w.stop()
	}
	return nil
}

type pacedPacket struct {
	hdr     rtp.Header
	payload []byte
	attrs   interceptor.Attributes
}

type pacedWriter struct {
	next   interceptor.RTPWriter
	ch     chan pacedPacket
	closed chan struct{}
	once   sync.Once
}

// Ёмкость очереди: ~1.5МБ — с запасом больше любого кейфрейма; при слive 7.5Мбит
// стекает за ~1.5с. Если Write упирается в полную очередь — блокируемся (давление
// назад на broadcast-цикл), а не дропаем: дроп RTP-пакета = битый кадр = PLI.
const pacedQueue = 1280

func newPacedWriter(next interceptor.RTPWriter) *pacedWriter {
	w := &pacedWriter{next: next, ch: make(chan pacedPacket, pacedQueue), closed: make(chan struct{})}
	go w.loop()
	return w
}

func (w *pacedWriter) stop() { w.once.Do(func() { close(w.closed) }) }

func (w *pacedWriter) Write(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
	// Копии обязательны: заголовок и payload переиспользуются пакетизатором
	// сразу после возврата, а слив произойдёт позже.
	pkt := pacedPacket{hdr: header.Clone(), payload: append([]byte(nil), payload...), attrs: attrs}
	select {
	case w.ch <- pkt:
		return header.MarshalSize() + len(payload), nil
	case <-w.closed:
		return 0, nil
	}
}

// loop сливает очередь с ограниченной скоростью: после каждого пакета дедлайн
// следующего сдвигается на size/rate. Пока темп подачи ниже rate — пакеты уходят
// сразу (без добавленной задержки); залп кадра размазывается ровно до rate.
func (w *pacedWriter) loop() {
	var next time.Time
	// Диагностика: раз в ~5с — фактический слив, пик очереди и суммарное время сна.
	// Если drain-kbps заметно НИЖЕ битрейта энкодера или очередь постоянно глубокая —
	// пейсер душит поток (например, из-за грубых таймеров) и сам создаёт фризы.
	var dBytes, dPkts, dQmax int
	var dSleep time.Duration
	dStat := time.Now()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-w.closed:
			return
		case <-tick.C:
			el := time.Since(dStat)
			log.Printf("pacer: 5с drain=%d kbps (%d пкт), очередь max=%d/%d, сон=%dмс",
				int(float64(dBytes)*8/1000/el.Seconds()), dPkts, dQmax, pacedQueue,
				int(dSleep.Milliseconds()))
			dBytes, dPkts, dQmax, dSleep, dStat = 0, 0, 0, 0, time.Now()
		case pkt := <-w.ch:
			if q := len(w.ch); q > dQmax {
				dQmax = q
			}
			now := time.Now()
			if next.After(now) {
				time.Sleep(next.Sub(now))
				dSleep += time.Since(now)
			} else {
				next = now
			}
			_, _ = w.next.Write(&pkt.hdr, pkt.payload, pkt.attrs)
			bps := pacerTargetBps.Load()
			if bps < 500_000 {
				bps = 500_000 // нижний предел, чтобы очередь всегда стекала
			}
			size := pkt.hdr.MarshalSize() + len(pkt.payload)
			next = next.Add(time.Duration(size*8) * time.Second / time.Duration(bps))
			dBytes += size
			dPkts++
		}
	}
}

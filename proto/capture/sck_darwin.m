//go:build darwin

#import <Foundation/Foundation.h>
#import <AppKit/AppKit.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#include <string.h>
#include <stdlib.h>

// Реализована в Go (//export goSCKFrame): принимает один BGRA-кадр.
extern void goSCKFrame(int handle, void *buf, int len);

// Старт SCStream трогает CoreGraphics/WindowServer, который в «голом» CLI не
// инициализирован (Assertion CGS_REQUIRE_INIT). NSApplicationLoad поднимает
// связь с WindowServer для не-бандл процесса. Делаем один раз.
static void sck_ensure_app(void) {
	static dispatch_once_t once;
	dispatch_once(&once, ^{
		NSApplicationLoad();
	});
}

// Синхронно получает доступный контент (SCShareableContent — асинхронный API).
static SCShareableContent *sck_fetch_content(void) {
	__block SCShareableContent *res = nil;
	dispatch_semaphore_t sem = dispatch_semaphore_create(0);
	[SCShareableContent
		getShareableContentExcludingDesktopWindows:NO
		                       onScreenWindowsOnly:YES
		                         completionHandler:^(SCShareableContent *c, NSError *e) {
			res = c;
			dispatch_semaphore_signal(sem);
		}];
	dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
	return res;
}

// sck_list_sources перечисляет доступные источники захвата через
// ScreenCaptureKit и возвращает JSON-строку {displays, windows, apps}.
// Вызывающий обязан free() результат.
char *sck_list_sources(void) {
	@autoreleasepool {
		NSDictionary *result = @{};
		SCShareableContent *content = sck_fetch_content();
		{
			if (content) {
				NSMutableArray *displays = [NSMutableArray array];
				for (SCDisplay *d in content.displays) {
					[displays addObject:@{
						@"id": @(d.displayID),
						@"width": @(d.width),
						@"height": @(d.height),
					}];
				}

				NSMutableArray *windows = [NSMutableArray array];
				for (SCWindow *w in content.windows) {
					NSString *title = w.title ?: @"";
					NSString *app = w.owningApplication.applicationName ?: @"";
					// Пропускаем безымянные служебные окна.
					if (title.length == 0 && app.length == 0) {
						continue;
					}
					[windows addObject:@{
						@"id": @(w.windowID),
						@"title": title,
						@"app": app,
						@"width": @((int)w.frame.size.width),
						@"height": @((int)w.frame.size.height),
					}];
				}

				NSMutableArray *apps = [NSMutableArray array];
				for (SCRunningApplication *a in content.applications) {
					NSString *name = a.applicationName ?: @"";
					if (name.length == 0) {
						continue;
					}
					[apps addObject:@{
						@"pid": @(a.processID),
						@"bundleId": a.bundleIdentifier ?: @"",
						@"name": name,
					}];
				}

				result = @{ @"displays": displays, @"windows": windows, @"apps": apps };
			}
		}

		NSData *data = [NSJSONSerialization dataWithJSONObject:result options:0 error:nil];
		NSString *json = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
		return strdup(json.UTF8String);
	}
}

// --- Захват выбранного источника ---

// Делегат потока: на каждый кадр копирует BGRA плотно (без stride-паддинга)
// и отдаёт его в Go через goSCKFrame.
@interface KatanaOutput : NSObject <SCStreamOutput, SCStreamDelegate>
@property (nonatomic) int handle;
@end

@implementation KatanaOutput
- (void)stream:(SCStream *)stream
	didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
	               ofType:(SCStreamOutputType)type {
	if (type != SCStreamOutputTypeScreen || !CMSampleBufferIsValid(sampleBuffer)) {
		return;
	}
	CVImageBufferRef px = CMSampleBufferGetImageBuffer(sampleBuffer);
	if (!px) {
		return;
	}
	CVPixelBufferLockBaseAddress(px, kCVPixelBufferLock_ReadOnly);
	size_t w = CVPixelBufferGetWidth(px);
	size_t h = CVPixelBufferGetHeight(px);
	size_t stride = CVPixelBufferGetBytesPerRow(px);
	uint8_t *base = (uint8_t *)CVPixelBufferGetBaseAddress(px);
	size_t rowBytes = w * 4;
	uint8_t *tight = malloc(rowBytes * h);
	if (tight && base) {
		for (size_t y = 0; y < h; y++) {
			memcpy(tight + y * rowBytes, base + y * stride, rowBytes);
		}
	}
	CVPixelBufferUnlockBaseAddress(px, kCVPixelBufferLock_ReadOnly);
	if (tight) {
		goSCKFrame(self.handle, tight, (int)(rowBytes * h));
		free(tight);
	}
}
- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
}
@end

static NSMutableDictionary<NSNumber *, SCStream *> *gStreams;
static NSMutableDictionary<NSNumber *, KatanaOutput *> *gOutputs;
static dispatch_queue_t gQueue;

// sck_source_size возвращает размер источника в пикселях (без старта потока),
// чтобы вызывающий мог настроить ffmpeg на нужный video_size.
// kind: 1=window, 2=app, иначе display. Возвращает 0 при успехе.
int sck_source_size(int kind, unsigned int sid, int *outW, int *outH) {
	@autoreleasepool {
		SCShareableContent *content = sck_fetch_content();
		if (!content) {
			return 1;
		}
		int w = 0, h = 0;
		if (kind == 1) {
			for (SCWindow *x in content.windows) {
				if (x.windowID == sid) {
					w = (int)x.frame.size.width;
					h = (int)x.frame.size.height;
					break;
				}
			}
		} else if (kind == 2) {
			SCDisplay *d = content.displays.firstObject;
			if (d) {
				w = (int)d.width;
				h = (int)d.height;
			}
		} else {
			for (SCDisplay *d in content.displays) {
				if (d.displayID == sid) {
					w = (int)d.width;
					h = (int)d.height;
					break;
				}
			}
		}
		if (w <= 0 || h <= 0) {
			return 2;
		}
		if (outW) *outW = w;
		if (outH) *outH = h;
		return 0;
	}
}

// sck_start запускает поток захвата выбранного источника. Кадры идут в
// goSCKFrame(handle, ...). Возвращает 0 при успехе.
int sck_start(int kind, unsigned int sid, int fps, int handle) {
	@autoreleasepool {
		sck_ensure_app();
		SCShareableContent *content = sck_fetch_content();
		if (!content) {
			return 1;
		}

		SCContentFilter *filter = nil;
		int w = 0, h = 0;
		if (kind == 1) { // окно
			SCWindow *win = nil;
			for (SCWindow *x in content.windows) {
				if (x.windowID == sid) { win = x; break; }
			}
			if (!win) return 2;
			filter = [[SCContentFilter alloc] initWithDesktopIndependentWindow:win];
			w = (int)win.frame.size.width;
			h = (int)win.frame.size.height;
		} else if (kind == 2) { // приложение
			SCRunningApplication *app = nil;
			for (SCRunningApplication *a in content.applications) {
				if (a.processID == (pid_t)sid) { app = a; break; }
			}
			if (!app) return 3;
			SCDisplay *disp = content.displays.firstObject;
			if (!disp) return 4;
			filter = [[SCContentFilter alloc] initWithDisplay:disp
			                            includingApplications:@[ app ]
			                                  exceptingWindows:@[]];
			w = (int)disp.width;
			h = (int)disp.height;
		} else { // дисплей
			SCDisplay *disp = nil;
			for (SCDisplay *d in content.displays) {
				if (d.displayID == sid) { disp = d; break; }
			}
			if (!disp) disp = content.displays.firstObject;
			if (!disp) return 5;
			filter = [[SCContentFilter alloc] initWithDisplay:disp excludingWindows:@[]];
			w = (int)disp.width;
			h = (int)disp.height;
		}
		if (w <= 0 || h <= 0) return 6;

		SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
		cfg.width = w;
		cfg.height = h;
		cfg.minimumFrameInterval = CMTimeMake(1, fps > 0 ? fps : 30);
		cfg.pixelFormat = kCVPixelFormatType_32BGRA;
		cfg.queueDepth = 5;
		cfg.showsCursor = YES;

		KatanaOutput *out = [[KatanaOutput alloc] init];
		out.handle = handle;
		SCStream *stream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:out];

		if (!gStreams) {
			gStreams = [NSMutableDictionary dictionary];
			gOutputs = [NSMutableDictionary dictionary];
			gQueue = dispatch_queue_create("katana.sck", DISPATCH_QUEUE_SERIAL);
		}

		NSError *addErr = nil;
		if (![stream addStreamOutput:out
		                        type:SCStreamOutputTypeScreen
		          sampleHandlerQueue:gQueue
		                       error:&addErr]) {
			return 7;
		}

		__block int startErr = 0;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[stream startCaptureWithCompletionHandler:^(NSError *e) {
			if (e) startErr = 8;
			dispatch_semaphore_signal(sem);
		}];
		dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
		if (startErr) return startErr;

		gStreams[@(handle)] = stream;
		gOutputs[@(handle)] = out;
		return 0;
	}
}

// sck_stop останавливает поток захвата.
void sck_stop(int handle) {
	@autoreleasepool {
		SCStream *stream = gStreams[@(handle)];
		if (!stream) return;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[stream stopCaptureWithCompletionHandler:^(NSError *e) {
			dispatch_semaphore_signal(sem);
		}];
		dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
		[gStreams removeObjectForKey:@(handle)];
		[gOutputs removeObjectForKey:@(handle)];
	}
}

// macOS implementation of the About panel and app-menu selector glue
// (issue #429). Declarations live in about_platform_darwin.go; this file uses
// //export, so its preamble stays declarations-only (cgo rule). Every entry
// point hops to the GCD main queue: NSAlert must run on the main thread and
// the NSApplication selector targets expect the main thread too.
//
// @joestump-agent 09/04/2026 - Added with #429.
#import <Cocoa/Cocoa.h>

void msgbrowse_show_about_panel(const char* title, const char* message) {
    NSString* t = [NSString stringWithUTF8String:title];
    NSString* m = [NSString stringWithUTF8String:message];
    dispatch_async(dispatch_get_main_queue(), ^{
        NSAlert* alert = [NSAlert new];
        [alert setAlertStyle:NSAlertStyleInformational];
        [alert setMessageText:t];
        [alert setInformativeText:m];
        [alert.window setLevel:NSFloatingWindowLevel];
        [alert runModal];
    });
}

void msgbrowse_hide_app(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        [[NSApplication sharedApplication] hide:nil];
    });
}

void msgbrowse_hide_others(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        [[NSApplication sharedApplication] hideOtherApplications:nil];
    });
}

void msgbrowse_show_all(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        [[NSApplication sharedApplication] unhideAllApplications:nil];
    });
}

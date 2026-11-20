/* setjmp/longjmp without exception handling: setjmp always returns 0 and
   longjmp traps, so a libvpx internal error ends the call with a trap. */
#ifndef KV_SETJMP_H
#define KV_SETJMP_H
typedef int jmp_buf[1];
#define setjmp(b) ((void)(b), 0)
#define longjmp(b, v) ((void)(b), (void)(v), __builtin_trap())
#endif

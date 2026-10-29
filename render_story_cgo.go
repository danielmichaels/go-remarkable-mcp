//go:build cgo && !nocgo

package main

/*
#include <stdlib.h>
#include <string.h>
#include <setjmp.h>
#include <stdio.h>
#include <stdint.h>

// Public ABI declarations for MuPDF 1.28.0, as bundled by go-fitz v1.28.2.
// Symbols and bundled fallback fonts come from go-fitz's linked static library.
// Keep this deliberately small ABI shim in sync when upgrading go-fitz.
typedef struct fz_context fz_context;
typedef struct fz_buffer fz_buffer;
typedef struct fz_story fz_story;
typedef struct fz_output fz_output;
typedef struct fz_document_writer fz_document_writer;
typedef struct fz_device fz_device;
typedef struct fz_xml fz_xml;
typedef struct fz_font fz_font;
typedef struct { float x0,y0,x1,y1; } fz_rect;
typedef struct { float a,b,c,d,e,f; } fz_matrix;
extern const char *fz_version;
extern fz_context *fz_new_context_imp(const void*, const void*, size_t, const char*);
extern void fz_drop_context(fz_context*);
extern jmp_buf *fz_push_try(fz_context*);
extern int fz_do_try(fz_context*);
extern int fz_do_catch(fz_context*);
extern const char *fz_caught_message(fz_context*);
extern fz_buffer *fz_new_buffer(fz_context*, size_t);
extern fz_buffer *fz_new_buffer_from_copied_data(fz_context*, const unsigned char*, size_t);
extern size_t fz_buffer_storage(fz_context*, fz_buffer*, unsigned char**);
extern void fz_drop_buffer(fz_context*, fz_buffer*);
extern fz_story *fz_new_story(fz_context*, fz_buffer*, const char*, float, void*);
extern int fz_place_story(fz_context*, fz_story*, fz_rect, fz_rect*);
extern void fz_draw_story(fz_context*, fz_story*, fz_device*, fz_matrix);
extern void fz_drop_story(fz_context*, fz_story*);
extern fz_output *fz_new_output_with_buffer(fz_context*, fz_buffer*);
extern void fz_drop_output(fz_context*, fz_output*);
extern fz_document_writer *fz_new_pdf_writer_with_output(fz_context*, fz_output*, const char*);
extern fz_device *fz_begin_page(fz_context*, fz_document_writer*, fz_rect);
extern void fz_end_page(fz_context*, fz_document_writer*);
extern void fz_close_document_writer(fz_context*, fz_document_writer*);
extern void fz_drop_document_writer(fz_context*, fz_document_writer*);
extern fz_xml *fz_parse_xml_from_html5(fz_context*, fz_buffer*);
extern void fz_drop_xml(fz_context*, fz_xml*);
extern fz_xml *fz_xml_down(fz_xml*);
extern fz_xml *fz_xml_next(fz_xml*);
extern char *fz_xml_tag(fz_xml*);
extern char *fz_xml_text(fz_xml*);
extern void fz_append_data(fz_context*, fz_buffer*, const void*, size_t);
extern void *fz_malloc(fz_context*, size_t);
extern char *fz_strdup(fz_context*, const char*);
extern void fz_free(fz_context*, void*);
typedef struct {
    int depth, heading;
    const char *id, *href;
    fz_rect rect;
    const char *text;
    int open_close, rectangle_num;
} fz_story_element_position;
extern void fz_story_positions(fz_context*, fz_story*, void (*)(fz_context*,void*,const fz_story_element_position*), void*);
typedef struct rm_position {
    struct rm_position *next;
    char *id, *href;
    fz_rect rect;
    int page;
} rm_position;
typedef struct { rm_position *head, *tail; int page; } rm_positions;
static void rm_collect_position(fz_context *ctx, void *arg, const fz_story_element_position *p) {
    if (!(p->open_close & 1) || (!p->id && !p->href)) return;
    rm_positions *list=arg;
    rm_position *node=fz_malloc(ctx,sizeof(*node));
    memset(node,0,sizeof(*node));
    if (list->tail) list->tail->next=node; else list->head=node;
    list->tail=node;
    node->rect=p->rect; node->page=list->page;
    if (p->id) node->id=fz_strdup(ctx,p->id);
    if (p->href) node->href=fz_strdup(ctx,p->href);
}
static fz_buffer *rm_story_links(fz_context*, fz_buffer*, rm_positions*);
extern fz_font *fz_new_font_from_memory(fz_context*, const char*, const unsigned char*, int, int, int);
extern void fz_install_load_system_font_funcs(fz_context*, void*, void*, fz_font *(*)(fz_context*,int,int,int,int,int));
extern int ucdn_get_script(unsigned int);
static const unsigned char *rm_cjk, *rm_arabic;
static int rm_cjk_len, rm_arabic_len;
static fz_font *rm_fallback(fz_context *ctx, int script, int language, int serif, int bold, int italic) {
    if (script == ucdn_get_script(0x0627))
        return fz_new_font_from_memory(ctx, "Noto Naskh Arabic", rm_arabic, rm_arabic_len, 0, 0);
    return fz_new_font_from_memory(ctx, "Droid Sans Fallback", rm_cjk, rm_cjk_len, 0, 0);
}

// HTML5 parser supplies character references and tolerant HTML parsing. Emit
// text-node boundaries, not visual line wraps, matching BeautifulSoup get_text.
static void rm_html_text(fz_context *ctx, fz_buffer *out, fz_xml *node, int in_body) {
    for (; node; node = fz_xml_next(node)) {
        const char *tag = fz_xml_tag(node);
        if (tag && (!strcmp(tag,"script") || !strcmp(tag,"style") || !strcmp(tag,"template"))) continue;
        const char *s = fz_xml_text(node);
        if (s && in_body) fz_append_data(ctx, out, s, strlen(s)+1);
        fz_xml *child=fz_xml_down(node);
        int body=tag && !strcmp(tag,"body");
        // ebooklib rebuilds <body> by appending its element children, which
        // discards body.text but retains each element's following tail text.
        if (body) while (child && fz_xml_text(child)) child=fz_xml_next(child);
        rm_html_text(ctx, out, child, in_body || body);
    }
}

static unsigned char *rm_native(const char *html, const char *css, int story_mode, size_t *len, char *error,
    const unsigned char *cjk, int cjk_len, const unsigned char *arabic, int arabic_len) {
    fz_context *ctx = fz_new_context_imp(NULL, NULL, 256 << 20, fz_version);
    if (!ctx) { strcpy(error,"cannot create MuPDF context"); return NULL; }
    rm_cjk = cjk; rm_cjk_len = cjk_len; rm_arabic = arabic; rm_arabic_len = arabic_len;
    fz_install_load_system_font_funcs(ctx, NULL, NULL, rm_fallback);
    fz_buffer *volatile input = NULL, *volatile out = NULL;
    fz_story *volatile story = NULL;
    fz_xml *volatile xml = NULL;
    fz_output *volatile output = NULL;
    fz_document_writer *volatile writer = NULL;
    unsigned char *volatile result = NULL;
    rm_positions *positions=calloc(1,sizeof(*positions));
    if (!positions) { strcpy(error,"cannot allocate Story positions"); fz_drop_context(ctx); return NULL; }
    if (!setjmp(*fz_push_try(ctx))) if (fz_do_try(ctx)) do {
        input = fz_new_buffer_from_copied_data(ctx, (const unsigned char*)html, strlen(html));
        out = fz_new_buffer(ctx, 4096);
        if (story_mode) {
            story = fz_new_story(ctx, input, css, 12, NULL);
            output = fz_new_output_with_buffer(ctx, out);
            writer = fz_new_pdf_writer_with_output(ctx, output, "compress=yes,garbage=3");
            output = NULL; // writer owns the output from here
            int more = 1, pages = 0;
            while (more && pages < 10000) {
                fz_device *device = fz_begin_page(ctx, writer, (fz_rect){0,0,595,842});
                fz_rect filled;
                more = fz_place_story(ctx, story, (fz_rect){54,54,541,788}, &filled);
                positions->page=pages;
                fz_story_positions(ctx,story,rm_collect_position,positions);
                fz_draw_story(ctx, story, device, (fz_matrix){1,0,0,1,0,0});
                fz_end_page(ctx, writer);
                pages++;
            }
            if (more) { strcpy(error,"Story exceeded 10000 page safety limit"); break; }
            fz_close_document_writer(ctx, writer);
            fz_buffer *linked=rm_story_links(ctx,out,positions);
            fz_drop_buffer(ctx,out); out=linked;
        } else {
            xml = fz_parse_xml_from_html5(ctx, input);
            rm_html_text(ctx, out, xml, 0);
        }
        unsigned char *data;
        *len = fz_buffer_storage(ctx, out, &data);
        result = malloc(*len ? *len : 1);
        if (result) memcpy(result, data, *len);
        else strcpy(error,"out of memory copying MuPDF result");
    } while (0);
    if (fz_do_catch(ctx)) snprintf(error, 512, "%s", fz_caught_message(ctx));
    fz_drop_document_writer(ctx, writer);
    fz_drop_output(ctx, output);
    fz_drop_story(ctx, story);
    fz_drop_xml(ctx, xml);
    fz_drop_buffer(ctx, input);
    fz_drop_buffer(ctx, out);
    rm_position *position=positions->head;
    while (position) {
        rm_position *next=position->next;
        fz_free(ctx,position->id); fz_free(ctx,position->href); fz_free(ctx,position);
        position=next;
    }
    free(positions);
    fz_drop_context(ctx);
    return result;
}

typedef struct fz_document fz_document;
typedef struct fz_page fz_page;
typedef struct fz_stream fz_stream;
typedef struct pdf_document pdf_document;
typedef struct pdf_obj pdf_obj;
extern fz_stream *fz_open_memory(fz_context*, const unsigned char*, size_t);
extern void fz_drop_stream(fz_context*, fz_stream*);
extern void fz_register_document_handlers(fz_context*);
extern fz_document *fz_open_document_with_stream(fz_context*, const char*, fz_stream*);
extern void fz_drop_document(fz_context*, fz_document*);
extern fz_page *fz_load_page(fz_context*, fz_document*, int);
extern fz_rect fz_bound_page(fz_context*, fz_page*);
extern void fz_run_page(fz_context*, fz_page*, fz_device*, fz_matrix, void*);
extern void fz_drop_page(fz_context*, fz_page*);
extern void fz_close_device(fz_context*, fz_device*);
extern void fz_drop_device(fz_context*, fz_device*);
extern pdf_document *pdf_create_document(fz_context*);
extern fz_device *pdf_page_write(fz_context*, pdf_document*, fz_rect, pdf_obj**, fz_buffer**);
extern pdf_obj *pdf_add_page(fz_context*, pdf_document*, fz_rect, int, pdf_obj*, fz_buffer*);
extern void pdf_insert_page(fz_context*, pdf_document*, int, pdf_obj*);
extern void pdf_drop_obj(fz_context*, pdf_obj*);
extern void pdf_drop_document(fz_context*, pdf_document*);
extern const char pdf_default_write_options[];
extern void pdf_write_document(fz_context*, pdf_document*, fz_output*, const void*);
extern int fz_lookup_metadata(fz_context*, fz_document*, const char*, char*, size_t);
extern void fz_set_metadata(fz_context*, fz_document*, const char*, const char*);
extern void fz_close_output(fz_context*, fz_output*);
typedef struct fz_link fz_link;
extern fz_link *fz_create_link(fz_context*, fz_page*, fz_rect, const char*);
extern void fz_drop_link(fz_context*, fz_link*);
extern void fz_rethrow(fz_context*);
extern void fz_throw(fz_context*, int, const char*, ...);
extern pdf_obj *pdf_new_dict(fz_context*, pdf_document*, int);
extern pdf_obj *pdf_new_array(fz_context*, pdf_document*, int);
extern pdf_obj *pdf_new_name(fz_context*, const char*);
extern pdf_obj *pdf_new_int(fz_context*, int64_t);
extern pdf_obj *pdf_new_text_string(fz_context*, const char*);
extern pdf_obj *pdf_new_rect(fz_context*, pdf_document*, fz_rect);
extern pdf_obj *pdf_dict_gets(fz_context*, pdf_obj*, const char*);
extern void pdf_dict_puts_drop(fz_context*, pdf_obj*, const char*, pdf_obj*);
extern pdf_obj *pdf_dict_puts_dict(fz_context*, pdf_obj*, const char*, int);
extern pdf_obj *pdf_add_object(fz_context*, pdf_document*, pdf_obj*);
extern void pdf_array_push(fz_context*, pdf_obj*, pdf_obj*);
extern pdf_obj *pdf_lookup_page_obj(fz_context*, pdf_document*, int);

// Reconstruct annotation rectangles from Story's actual placed elements,
// including wrapped and cross-page links. Resolve forward #id references only
// after every page has been laid out, just like Story.write_with_links.
static fz_buffer *rm_story_links(fz_context *ctx, fz_buffer *pdf, rm_positions *positions) {
    fz_stream *volatile stream=NULL;
    fz_document *volatile doc=NULL;
    fz_page *volatile page=NULL;
    fz_output *volatile output=NULL;
    fz_buffer *volatile out=NULL;
    pdf_obj *volatile annotation=NULL, *volatile reference=NULL;
    if (!setjmp(*fz_push_try(ctx))) if (fz_do_try(ctx)) do {
        unsigned char *data;
        size_t len=fz_buffer_storage(ctx,pdf,&data);
        fz_register_document_handlers(ctx);
        stream=fz_open_memory(ctx,data,len);
        doc=fz_open_document_with_stream(ctx,"application/pdf",stream);
        for (rm_position *p=positions->head;p;p=p->next) {
            if (!p->href || !p->href[0]) continue;
            const char *uri=p->href;
            char destination[160];
            page=fz_load_page(ctx,doc,p->page);
            if (uri[0]=='#') {
                rm_position *target=positions->head;
                while (target && (!target->id || strcmp(target->id,uri+1))) target=target->next;
                if (!target) fz_throw(ctx,4,"No destination with id=%s",uri+1);
                snprintf(destination,sizeof(destination),"#page=%d&zoom=100,%g,%g",target->page+1,target->rect.x0,target->rect.y0);
                fz_link *link=fz_create_link(ctx,page,p->rect,destination);
                fz_drop_link(ctx,link);
            } else {
                // Python uses URI actions even for relative paths, and GoTo
                // named destinations for name:. The generic MuPDF link URI
                // parser otherwise rejects or reclassifies these strings.
                pdf_document *pdfdoc=(pdf_document*)doc;
                annotation=pdf_new_dict(ctx,pdfdoc,4);
                pdf_dict_puts_drop(ctx,annotation,"Subtype",pdf_new_name(ctx,"Link"));
                fz_rect box=fz_bound_page(ctx,page);
                fz_rect rect={p->rect.x0,box.y1-p->rect.y1,p->rect.x1,box.y1-p->rect.y0};
                pdf_dict_puts_drop(ctx,annotation,"Rect",pdf_new_rect(ctx,pdfdoc,rect));
                pdf_obj *border=pdf_dict_puts_dict(ctx,annotation,"BS",1);
                pdf_dict_puts_drop(ctx,border,"W",pdf_new_int(ctx,0));
                pdf_obj *action=pdf_dict_puts_dict(ctx,annotation,"A",2);
                int named=!strncmp(uri,"name:",5);
                pdf_dict_puts_drop(ctx,action,"S",pdf_new_name(ctx,named ? "GoTo" : "URI"));
                pdf_dict_puts_drop(ctx,action,named ? "D" : "URI",pdf_new_text_string(ctx,named ? uri+5 : uri));
                pdf_obj *pageobj=pdf_lookup_page_obj(ctx,pdfdoc,p->page);
                pdf_obj *annots=pdf_dict_gets(ctx,pageobj,"Annots");
                if (!annots) {
                    annots=pdf_new_array(ctx,pdfdoc,4);
                    pdf_dict_puts_drop(ctx,pageobj,"Annots",annots);
                }
                reference=pdf_add_object(ctx,pdfdoc,annotation);
                pdf_array_push(ctx,annots,reference);
                pdf_drop_obj(ctx,reference); reference=NULL;
                pdf_drop_obj(ctx,annotation); annotation=NULL;
            }
            fz_drop_page(ctx,page); page=NULL;
        }
        out=fz_new_buffer(ctx,4096);
        output=fz_new_output_with_buffer(ctx,out);
        pdf_write_document(ctx,(pdf_document*)doc,output,pdf_default_write_options);
        fz_close_output(ctx,output);
    } while(0);
    if (fz_do_catch(ctx)) {
        pdf_drop_obj(ctx,reference); pdf_drop_obj(ctx,annotation);
        fz_drop_page(ctx,page); fz_drop_document(ctx,doc); fz_drop_stream(ctx,stream);
        fz_drop_output(ctx,output); fz_drop_buffer(ctx,out);
        fz_rethrow(ctx);
    }
    fz_drop_page(ctx,page); fz_drop_document(ctx,doc); fz_drop_stream(ctx,stream);
    fz_drop_output(ctx,output);
    return out;
}

typedef struct { const char *svg; int index; float width, height, ppu; } rm_export_page;
static unsigned char *rm_export_pdf(const unsigned char *source, size_t source_len,
    rm_export_page *pages, int count, const char **metadata, size_t *len, char *error) {
    fz_context *ctx = fz_new_context_imp(NULL,NULL,256<<20,fz_version);
    if (!ctx) { strcpy(error,"cannot create MuPDF context"); return NULL; }
    fz_document *volatile original=NULL, *volatile overlay=NULL;
    pdf_document *volatile doc=NULL;
    fz_stream *volatile stream=NULL;
    fz_page *volatile page=NULL, *volatile ink=NULL;
    fz_device *volatile device=NULL;
    fz_buffer *volatile contents=NULL, *volatile out=NULL;
    pdf_obj *volatile resources=NULL, *volatile obj=NULL;
    fz_output *volatile output=NULL;
    unsigned char *volatile result=NULL;
    char *volatile existing=NULL, *volatile joined=NULL;
    if (!setjmp(*fz_push_try(ctx))) if (fz_do_try(ctx)) do {
        fz_register_document_handlers(ctx);
        doc=pdf_create_document(ctx);
        if (source_len) {
            stream=fz_open_memory(ctx,source,source_len);
            original=fz_open_document_with_stream(ctx,"application/pdf",stream);
            fz_drop_stream(ctx,stream); stream=NULL;
        }
        fz_document *metadata_source=original;
        if (count==0 && original) {
            pdf_drop_document(ctx,doc);
            doc=(pdf_document*)original;
            original=NULL; // doc owns the reference, including on exceptions
        }
        const char *keys[]={"info:Title","info:Subject","info:Keywords","info:Creator","info:Producer"};
        for (int k=0;k<5;k++) {
            int size=metadata_source ? fz_lookup_metadata(ctx,metadata_source,keys[k],NULL,0) : 0;
            existing=calloc(size>0 ? size : 1,1);
            if (!existing) { strcpy(error,"metadata allocation failed"); break; }
            if (size>0) fz_lookup_metadata(ctx,metadata_source,keys[k],existing,size);
            if (metadata[k] && metadata[k][0]) {
                if ((k==1 || k==2) && existing[0] && strcmp(existing,metadata[k])) {
                    size_t n=strlen(existing)+strlen(metadata[k])+3;
                    joined=malloc(n);
                    if (!joined) { strcpy(error,"metadata allocation failed"); break; }
                    snprintf(joined,n,"%s; %s",existing,metadata[k]);
                    fz_set_metadata(ctx,(fz_document*)doc,keys[k],joined);
                    free(joined); joined=NULL;
                } else fz_set_metadata(ctx,(fz_document*)doc,keys[k],metadata[k]);
            } else if (existing[0]) fz_set_metadata(ctx,(fz_document*)doc,keys[k],existing);
            free(existing); existing=NULL;
        }
        if (error[0]) break;
        for (int i=0;i<count;i++) {
            fz_rect box={0,0,pages[i].width/2,pages[i].height/2};
            if (original && pages[i].index>=0) {
                page=fz_load_page(ctx,original,pages[i].index);
                box=fz_bound_page(ctx,page);
            }
            device=pdf_page_write(ctx,doc,box,(pdf_obj**)&resources,(fz_buffer**)&contents);
            if (page) fz_run_page(ctx,page,device,(fz_matrix){1,0,0,1,0,0},NULL);
            stream=fz_open_memory(ctx,(const unsigned char*)pages[i].svg,strlen(pages[i].svg));
            overlay=fz_open_document_with_stream(ctx,"image/svg+xml",stream);
            fz_drop_stream(ctx,stream); stream=NULL;
            ink=fz_load_page(ctx,overlay,0);
            float scale=page ? pages[i].ppu : 0.5f;
            float dx=page ? (box.x1-box.x0-pages[i].width*scale)/2 : 0;
            fz_run_page(ctx,ink,device,(fz_matrix){scale,0,0,scale,dx,0},NULL);
            fz_close_device(ctx,device); fz_drop_device(ctx,device); device=NULL;
            obj=pdf_add_page(ctx,doc,box,0,resources,contents);
            pdf_insert_page(ctx,doc,-1,obj);
            pdf_drop_obj(ctx,obj); obj=NULL;
            pdf_drop_obj(ctx,resources); resources=NULL;
            fz_drop_buffer(ctx,contents); contents=NULL;
            fz_drop_page(ctx,page); page=NULL;
            fz_drop_page(ctx,ink); ink=NULL;
            fz_drop_document(ctx,overlay); overlay=NULL;
        }
        out=fz_new_buffer(ctx,4096);
        output=fz_new_output_with_buffer(ctx,out);
        pdf_write_document(ctx,doc,output,pdf_default_write_options);
        fz_close_output(ctx,output);
        unsigned char *data;
        *len=fz_buffer_storage(ctx,out,&data);
        result=malloc(*len ? *len : 1);
        if (result) memcpy(result,data,*len); else strcpy(error,"export allocation failed");
    } while(0);
    if (fz_do_catch(ctx)) snprintf(error,512,"%s",fz_caught_message(ctx));
    fz_drop_device(ctx,device);
    fz_drop_page(ctx,page); fz_drop_page(ctx,ink);
    fz_drop_document(ctx,overlay); fz_drop_document(ctx,original);
    fz_drop_stream(ctx,stream);
    pdf_drop_obj(ctx,obj); pdf_drop_obj(ctx,resources);
    fz_drop_buffer(ctx,contents); fz_drop_output(ctx,output); fz_drop_buffer(ctx,out);
    pdf_drop_document(ctx,doc);
    free(existing); free(joined);
    fz_drop_context(ctx);
    return result;
}
*/
import "C"

import (
	_ "embed"
	"fmt"
	"strings"
	"unsafe"
)

// MuPDF's go-fitz build omits the large fallback fonts. Bundle the same
// upstream faces used by its full build, without requiring installed fonts.
// Sources: https://github.com/ArtifexSoftware/mupdf/tree/1.28.0/resources/fonts
// droid/DroidSansFallbackFull.ttf SHA-256:
// 8a4dea0899424438af25a6f1f6eb61e5d111d85367eece0a5d30170861ae6b2e
// noto/NotoNaskhArabic-Regular.otf SHA-256:
// 21d2fa0dc32187333d3a0861361cd8c6e8c0563c8d880ebee2cbd1b8f79a553c
//
//go:embed render_fonts/DroidSansFallbackFull.ttf
var storyCJK []byte

//go:embed render_fonts/NotoNaskhArabic-Regular.otf
var storyArabic []byte

func nativeHTML(html, css string, story bool) ([]byte, error) {
	nativeFitzMu.Lock()
	defer nativeFitzMu.Unlock()
	h, c := C.CString(html), C.CString(css)
	defer C.free(unsafe.Pointer(h))
	defer C.free(unsafe.Pointer(c))
	var size C.size_t
	var message [512]C.char
	mode := C.int(0)
	if story {
		mode = 1
	}
	cjk, arabic := C.CBytes(storyCJK), C.CBytes(storyArabic)
	defer C.free(cjk)
	defer C.free(arabic)
	data := C.rm_native(h, c, mode, &size, &message[0], (*C.uchar)(cjk), C.int(len(storyCJK)), (*C.uchar)(arabic), C.int(len(storyArabic)))
	defer C.free(unsafe.Pointer(data))
	if message[0] != 0 {
		return nil, fmt.Errorf("MuPDF: %s", C.GoString(&message[0]))
	}
	if uint64(size) > 1<<30 {
		return nil, fmt.Errorf("MuPDF output exceeds 1 GiB")
	}
	return C.GoBytes(unsafe.Pointer(data), C.int(size)), nil
}

func nativeStoryPDF(html, css string) ([]byte, error) { return nativeHTML(html, css, true) }

func nativeEPUBHTMLText(html []byte) (string, error) {
	data, err := nativeHTML(string(html), "", false)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, part := range strings.Split(string(data), "\x00") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func nativeVectorExport(source []byte, pages []vectorExportPage, metadata map[string]string) ([]byte, error) {
	nativeFitzMu.Lock()
	defer nativeFitzMu.Unlock()
	data := C.CBytes(source)
	defer C.free(data)
	ptr := C.malloc(C.size_t(len(pages)) * C.size_t(C.sizeof_rm_export_page))
	defer C.free(ptr)
	array := unsafe.Slice((*C.rm_export_page)(ptr), len(pages))
	for i, p := range pages {
		array[i].svg = C.CString(p.svg)
		defer C.free(unsafe.Pointer(array[i].svg))
		array[i].index = C.int(p.index)
		array[i].width = C.float(p.width)
		array[i].height = C.float(p.height)
		array[i].ppu = C.float(p.ppu)
	}
	var values [5]*C.char
	for i, k := range []string{"title", "subject", "keywords", "creator", "producer"} {
		values[i] = C.CString(metadata[k])
		defer C.free(unsafe.Pointer(values[i]))
	}
	var size C.size_t
	var message [512]C.char
	result := C.rm_export_pdf((*C.uchar)(data), C.size_t(len(source)), (*C.rm_export_page)(ptr), C.int(len(pages)), &values[0], &size, &message[0])
	defer C.free(unsafe.Pointer(result))
	if message[0] != 0 {
		return nil, fmt.Errorf("MuPDF PDF export: %s", C.GoString(&message[0]))
	}
	if uint64(size) > 1<<30 {
		return nil, fmt.Errorf("PDF export exceeds 1 GiB")
	}
	return C.GoBytes(unsafe.Pointer(result), C.int(size)), nil
}

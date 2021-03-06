//    Micro version 0.4
//    Micro is a web framework for the Go language
//    Copyright (C) 2015  mparaiso <mparaiso@online.fr>
//
//    This program is free software: you can redistribute it and/or modify
//    it under the terms of the GNU General Public License as published by
//    the Free Software Foundation, either version 3 of the License, or
//    (at your option) any later version.

//    This program is distributed in the hope that it will be useful,
//    but WITHOUT ANY WARRANTY; without even the implied warranty of
//    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//    GNU General Public License for more details.

//    You should have received a copy of the GNU General Public License
//    along with this program.  If not, see <http://www.gnu.org/licenses/>

package micro

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
)

var (
	// Pattern represents a route param regexp pattern
	Pattern = "(?:\\:)(\\w+)(\\?)?|(\\(.+\\)?)"
	// DefaultParamPattern represents the default pattern that a route param matches
	DefaultParamPattern = "(\\w+)"
)

/**********************************/
/*               APP              */
/**********************************/

// Micro represents an micro application
type Micro struct {
	debug bool
	*ControllerCollection
	*EventEmitter
	RequestMatcher *RequestMatcher
	booted         bool
	injector       *Injector
	errorHandlers  map[int]HandlerFunction
}

// New creates an micro application
func New() *Micro {
	micro := &Micro{
		ControllerCollection: NewControllerCollection(),
		EventEmitter:         NewEventEmitter(),
		injector:             NewInjector(),
		errorHandlers:        map[int]HandlerFunction{},
	}
	micro.injector.Register(micro)
	return micro
}

// Boot boots the application
func (e *Micro) Boot() {
	if !e.Booted() {
		e.ControllerCollection.Flush()
		e.booted = true
	}
}

// Booted returns true if the Boot function has been called
func (e Micro) Booted() bool {
	return e.booted
}

// ServeHTTP boots micro server and handles http requests.
//
// Can Panic!
func (e *Micro) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	var (
		matches                []*Route
		next                   Next
		context                *Context
		requestInjector        *Injector
		responseWriterWithCode *ResponseWriterWithCode
	)
	defer func() {
		if err := recover(); err != nil {
			responseWriter.WriteHeader(http.StatusInternalServerError)
			log.Println(err)
			debug.PrintStack()
			requestInjector.MustApply(e.errorHandlers[500])
		}
	}()
	// wrap responseWriter so we can access the status code
	responseWriterWithCode = &ResponseWriterWithCode{
		ResponseWriter: responseWriter,
	}
	// sets context and injector
	context = NewContext(responseWriterWithCode, request)
	requestInjector = NewInjector(request, responseWriterWithCode, context, e.EventEmitter)
	requestInjector.Register(requestInjector)
	requestInjector.SetParent(e.Injector())
	if e.errorHandlers[500] == nil {
		e.Error(500, InternalServerErrorHandler)
	}
	if e.errorHandlers[404] == nil {
		e.Error(404, NotFoundErrorHandler)
	}
	if e.RequestMatcher == nil {
		e.RequestMatcher = NewRequestMatcher(e.ControllerCollection)
	}
	if !e.Booted() {
		e.Boot()
	}
	// find all routes matching the request in the route collection
	matches = e.RequestMatcher.MatchAll(request)

	// For the first matched route, call all its handlers
	// if an handler in a route calls micro.Next next() , execute the next handler
	// When all handlers of a route have been called
	// if there are still some matched routes and the last handler of the previous route calls next
	// then repeat the process for the next matched route
	next = func() {
		if e.hasErrorCode(responseWriterWithCode, requestInjector) {
			return
		}
		if len(matches) == 0 {
			requestInjector.MustApply(e.errorHandlers[404])
			return
		}
		match := matches[0]
		matches = matches[1:]
		// If there are some request variables, populate the context with them
		for i, matchedParam := range match.pattern.FindStringSubmatch(request.URL.Path)[1:] {
			context.RequestVars[match.params[i]] = matchedParam
		}

		requestInjector.Register(next)
		context.next = next
		requestInjector.MustApply(match.Handler())
	}
	next()

}

// Error sets an error handler given an error code.
// Arguments of that handler function are resolved by micro's injector.
//
// Can Panic! if the error code is lower than 400.
func (e *Micro) Error(errorCode int, handlerFunc HandlerFunction) {
	if e.Booted() {
		return
	}
	if errorCode < 400 {
		panic(fmt.Sprintf("errorCode should be greater or equal to 400, got %d", errorCode))
	}
	e.errorHandlers[errorCode] = handlerFunc
}

// hasErrorCode Return true if a http status greater than 399 has been set
func (e *Micro) hasErrorCode(rw *ResponseWriterWithCode, injector *Injector) bool {
	if code := rw.Code(); code > 399 {
		if e.errorHandlers[code] != nil && rw.Length() == 0 {
			injector.MustApply(e.errorHandlers[code])
		} else {
			http.Error(rw, http.StatusText(code), code)
		}
		return true
	}
	return false
}

// Injector return the injector
func (e *Micro) Injector() *Injector {
	return e.injector
}

/**********************************/
/*     DEFAULT ERROR HANDLERS     */
/**********************************/

// InternalServerErrorHandler executes the default 500 handler
func InternalServerErrorHandler(rw http.ResponseWriter) {
	rw.Write([]byte(http.StatusText(http.StatusInternalServerError)))
}

// NotFoundErrorHandler executes the default 404 handler
func NotFoundErrorHandler(rw http.ResponseWriter, r *http.Request) {
	http.NotFound(rw, r)
}

/**********************************/
/*            CONTEXT             */
/**********************************/

// Context represents a request context in an micro application
type Context struct {
	Request  *http.Request
	Response http.ResponseWriter
	// RequestVars are variables extracted from the request
	RequestVars          map[string]string
	//  Vars is a map to store any data during the request response cycle
	Vars map[string]interface{}
	next Next
}

// NewContext returns a new Context
func NewContext(response http.ResponseWriter, request *http.Request) *Context {
	ctx := &Context{
		RequestVars:          map[string]string{},
		Vars:                 map[string]interface{}{},
		Request:              request,
		Response:             response,
	}
	return ctx
}

// Next calls the next middleware in the middleware chain
func (ctx *Context) Next() {
	ctx.next()
}

// Redirect redirects request
func (ctx *Context) Redirect(path string, code int) {
	http.Redirect(ctx.Response, ctx.Request, path, code)
}

// WriteJSON writes json to response
func (ctx *Context) WriteJSON(v interface{}) error {
	ctx.Response.Header().Add("Content-Type", "application/json")
	return json.NewEncoder(ctx.Response).Encode(v)
}

// WriteXML writes xml to response
func (ctx *Context) WriteXML(v interface{}) error {
	ctx.Response.Header().Add("Content-Type", "text/xml")
	return xml.NewEncoder(ctx.Response).Encode(v)
}

// WriteString writes a string to response
func (ctx *Context) WriteString(v ...interface{}) (int, error) {
	return fmt.Fprint(ctx.Response, v...)
}

// WriteJSONP writes a jsonp response
func (ctx *Context) WriteJSONP(v interface{}, callbackName string) (n int, err error) {
	ctx.Response.Header().Add("Content-Type", "application/x-javascript")
	bytes, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	n, err = ctx.WriteString(callbackName+"(", bytes, ")")
	return
}

// ReadJSON reads json from request's Body
func (ctx *Context) ReadJSON(v interface{}) error {
	return json.NewDecoder(ctx.Request.Body).Decode(v)
}

// ReadXML reads xml from request's body
func (ctx *Context) ReadXML(v interface{}) error {
	return xml.NewDecoder(ctx.Request.Body).Decode(v)
}

/**********************************/
/*             ROUTE              */
/**********************************/

//Route represents a route in the router
type Route struct {
	// methods handled by the route
	methods []string
	// pattern is the pattern with which the request will be matched against
	pattern *regexp.Regexp
	// path is the path as string
	path        string
	handlerFunc HandlerFunction
	params      []string
	frozen      bool
	assertions  map[string]string
	attributes  map[string]interface{}
	// name is the route's name
	name string
	// wether the route is intended to be a middlware or not
	passthrough bool
	matchers    []Matcher
}

// NewRoute creates a new route with a path that handles all methods
func NewRoute(path string) *Route {
	return &Route{
		methods:     []string{},
		params:      []string{},
		assertions:  map[string]string{},
		attributes:  map[string]interface{}{},
		path:        path,
		handlerFunc: []HandlerFunction{},
	}
}

// SetName sets the route name
func (r *Route) SetName(name string) *Route {
	if r.IsFrozen() {
		return r
	}
	r.name = name
	return r
}

// Name returns the route's name
func (r *Route) Name() string {
	return r.name
}

// Params return route variable names.
// For instance if a route has the following pattern:
//    /catalog/:category/:productId
// it will return []string{"category","productId"}
func (r *Route) Params() []string { return r.params }

// Handler returns the current route handler function
func (r *Route) Handler() HandlerFunction {
	return r.handlerFunc
}

// HandlerFunction represent a route handler
type HandlerFunction interface{}

// SetHandler sets the route handler function.
//
// Can Panic!
func (r *Route) SetHandler(handlerFunc HandlerFunction) {
	if r.IsFrozen() {
		return
	}
	MustBeCallable(handlerFunc)
	r.handlerFunc = handlerFunc
}

// MethodMatch returns true if that method is handled by the route
//func (r Route) MethodMatch(method string) bool {
//	match := false
//	for _, m := range r.Methods() {
//		if strings.TrimSpace(strings.ToUpper(method)) == m || m == "*" {
//			match = true
//			break
//		}
//	}
//	return match
//}

// Freeze freezes a route , which will make it read only
func (r *Route) freeze() *Route {
	if r.IsFrozen() {
		return r
	}
	// extract route variables
	routeVarsRegexp := regexp.MustCompile(Pattern)
	matches := routeVarsRegexp.FindAllStringSubmatch(r.path, -1)
	if matches != nil && len(matches) > 0 {
		for i, match := range matches {
			if match[0][0] == ':' {
				// looks like a :param use param without :
				r.params = append(r.params, match[1])
			} else {
				// looks like a valid regexp group, use the param position instead as key
				r.params = append(r.params, fmt.Sprintf("%d", i))
			}
		}
	}
	// replace route variables either with the default variable pattern or an assertion corresponding to the route variable
	stringPattern := routeVarsRegexp.ReplaceAllStringFunc(r.path, func(match string) string {
		// if an assertion is found, replace with the assertion pattern
		params := regexp.MustCompile("\\w+").FindAllString(match, -1)
		if len(params) > 0 {
			if r.assertions[params[0]] != "" {
				// optional ?
				if strings.HasSuffix(match, "?") {
					return "?" + r.assertions[params[0]] + "?"
				}
				return r.assertions[params[0]]
			}
		}
		//if match looks like a valid regexp group, return match untouched
		if match[0] == '(' && match[len(match)-1] == ')' {
			return match
		}
		//if match ends with ? , match is optional
		if strings.HasSuffix(match, "?") {
			return "?" + DefaultParamPattern + "?"
		}
		return DefaultParamPattern
	})
	// add ^ and $ and optional /? to string pattern
	if strings.HasSuffix(stringPattern, "/") {
		stringPattern = "^" + stringPattern + "?"
	} else {
		stringPattern = "^" + stringPattern + "/?"
	}
	if !r.passthrough {
		stringPattern = stringPattern + "$"
	}
	r.pattern = regexp.MustCompile(stringPattern)
	if r.name == "" {
		r.name = regexp.MustCompile("\\W+").ReplaceAllString(r.path+"_"+fmt.Sprint(r.methods), "_")
	}
	r.matchers = []Matcher{
		NewPatternMatcher(r.pattern),
		NewMethodMatcher(r.Methods()...),
	}
	r.frozen = true

	return r
}

// IsFrozen return the frozen state of a route.
// A Frozen route cannot be modified.
func (r *Route) IsFrozen() bool {
	return r.frozen
}

// Methods gets methods handled by the route
func (r *Route) Methods() []string {
	return r.methods

}

// SetMethods sets the methods handled by the route.
//
// Example:
//
//    route.SetMethods([]string{"GET","POST"})
// []string{"*"} means the route handles all methods.
func (r *Route) SetMethods(methods []string) {
	if r.IsFrozen() == true {
		return
	}
	r.methods = methods
}

// Assert asserts that a route variable respects a given regexp pattern.
//
// WILL Panic! if the pattern is not valid regexp pattern
func (r *Route) Assert(parameterName string, pattern string) *Route {
	if r.IsFrozen() {
		return r
	}
	// if the pattern is not a valid regexp pattern string, panic
	regexp.MustCompile("(" + pattern + ")")
	r.assertions[parameterName] = "(" + pattern + ")"
	return r
}

// SetAttribute sets a route attribute
func (r *Route) SetAttribute(attr string, value interface{}) *Route {
	r.attributes[attr] = value
	return r
}

// Attribute returns a route attribute
func (r *Route) Attribute(attr string) interface{} {
	return r.attributes[attr]
}

/**********************************/
/*   CONTROLLER COLLECTION             */
/**********************************/

// ControllerCollection is a collection of routes
type ControllerCollection struct {
	Routes    []*Route
	prefix    string
	frozen    bool
	Children  []*ControllerCollection
	hasParent bool
}

// NewControllerCollection creates a new ControllerCollection
func NewControllerCollection() *ControllerCollection {
	return &ControllerCollection{Routes: []*Route{}, Children: []*ControllerCollection{}}
}

// AddRoute adds a route to the route collection
func (rc *ControllerCollection) AddRoute(r *Route) *ControllerCollection {
	rc.Routes = append(rc.Routes, r)
	return rc
}

func (rc *ControllerCollection) mustNotBeFrozen() {
	if rc.frozen {
		log.Panic("You cannot modify a route collection that has been frozen ", rc)
	}
}

func (rc *ControllerCollection) setPrefix(prefix string) *ControllerCollection {
	rc.mustNotBeFrozen()
	if prefix != "" {
		if prefix[0] != '/' {
			prefix = "/" + prefix
		}
		if strings.HasSuffix(prefix, "/") {
			prefix = prefix + "?"
		}
	}

	rc.prefix = prefix
	return rc
}

// Flush freezes a route collection
func (rc *ControllerCollection) Flush() {

	if rc.IsFrozen() == true {
		return
	}

	for _, route := range rc.Routes {
		route.path = rc.prefix + route.path
		route.freeze()
	}

	if len(rc.Children) > 0 {

		for _, routeCollection := range rc.Children {
			routeCollection.setPrefix(rc.prefix + routeCollection.prefix).Flush()
			for _, route := range routeCollection.Routes {
				rc.Routes = append(rc.Routes, route)
			}
			routeCollection.Routes = []*Route{}
		}
	}
	rc.frozen = true
}

// IsFrozen returns true if the route collection is frozen
func (rc ControllerCollection) IsFrozen() bool {
	return rc.frozen
}

// Use creates a passthrough route usefull for middlewares
func (rc *ControllerCollection) Use(path string, handlerFunction HandlerFunction) *Route {
	route := rc.All(path, handlerFunction)
	route.passthrough = true
	return route
}

// Mount mounts a route collection on a path. All routes in the route collection will be prefixed
// with that path.
func (rc *ControllerCollection) Mount(path string, routeCollection *ControllerCollection) *ControllerCollection {
	if !routeCollection.hasParent {

		rc.Children = append(rc.Children, routeCollection)
		routeCollection.setPrefix(path)
		routeCollection.hasParent = true
	}
	return rc
}

// Get creates a GET route
func (rc *ControllerCollection) Get(path string, handlerFunction HandlerFunction) *Route {
	route := rc.All(path, handlerFunction)
	route.SetMethods([]string{"GET", "HEAD"})
	return route
}

// Post creates a POST route
func (rc *ControllerCollection) Post(path string, handlerFunction HandlerFunction) *Route {
	route := rc.All(path, handlerFunction)
	route.SetMethods([]string{"POST"})
	return route
}

// Put creates a PUT route
func (rc *ControllerCollection) Put(path string, handlerFunction HandlerFunction) *Route {
	route := rc.All(path, handlerFunction)
	route.SetMethods([]string{"PUT"})
	return route
}

// Delete creates a DELETE route
func (rc *ControllerCollection) Delete(path string, handlerFunction HandlerFunction) *Route {
	route := rc.All(path, handlerFunction)
	route.SetMethods([]string{"DELETE"})
	return route
}

// All creates a route that matches all methods
func (rc *ControllerCollection) All(path string, handlerFunction HandlerFunction) *Route {
	rc.mustNotBeFrozen()
	route := NewRoute(path)
	route.SetHandler(handlerFunction)
	rc.Routes = append(rc.Routes, route)
	return route
}

/**********************************/
/*            MATCHERS            */
/**********************************/

// Matcher is a type something that can match a http.Request
type Matcher interface {
	Match(*http.Request) bool
}

// RequestMatcher match request path to route pattern
type RequestMatcher struct {
	routeCollection *ControllerCollection
}

// NewRequestMatcher returns a new RequestMatcher
func NewRequestMatcher(routeCollection *ControllerCollection) *RequestMatcher {
	return &RequestMatcher{routeCollection}
}

// MatchAll matches all routes matching the request in the route collection
func (rm *RequestMatcher) MatchAll(request *http.Request) (matches []*Route) {
	if len(rm.routeCollection.Routes) > 0 {
		for _, route := range rm.routeCollection.Routes {
			match := true
			for _, matcher := range route.matchers {
				if !matcher.Match(request) {
					match = false
					break
				}
			}
			if match == true {
				matches = append(matches, route)
			}
		}
	}
	return
}



/**********************************/
/*         EVENT EMITTER          */
/**********************************/

// Listener is an event handler function
type Listener *func(string, ...interface{}) bool

// EventEmitter listens for and emits events
type EventEmitter struct {
	handlers map[string][]Listener
}

// NewEventEmitter returns a new event emitter
func NewEventEmitter() *EventEmitter {
	return &EventEmitter{
		handlers: map[string][]Listener{},
	}
}

// Emit emits an event
func (em *EventEmitter) Emit(event string, arguments ...interface{}) {
	if len(em.handlers) > 0 && em.handlers[event] != nil {
		for _, handler := range em.handlers[event] {
			Continue := (*handler)(event, arguments...)
			if !Continue {
				break
			}
		}
	}
}

// AddListener adds a new listener function pointer
func (em *EventEmitter) AddListener(event string, listener Listener) {
	if em.handlers[event] != nil {
		em.handlers[event] = []Listener{}
	}
	em.handlers[event] = append(em.handlers[event], listener)
}

// RemoveListener removes a listener function pointer
func (em *EventEmitter) RemoveListener(event string, listener Listener) bool {
	var found bool
	if em.handlers[event] != nil {
		for i, handler := range em.handlers[event] {
			if handler == listener {

				head := em.handlers[event][0:i]
				if length := len(em.handlers); i == length-1 {
					em.handlers[event] = head
				} else {
					tail := em.handlers[event][i+1 : length-1]
					em.handlers[event] = append(head, tail...)
				}

				found = true
				break
			}
		}
	}
	return found
}

// RemoveAllListeners remove all listeners given an event and returns the listener slice
func (em *EventEmitter) RemoveAllListeners(event string) []Listener {
	listeners := []Listener{}
	if em.handlers[event] != nil {
		listeners, em.handlers[event] = em.handlers[event], listeners
	}
	return listeners
}

// HasListener returns true if an event has listeners
func (em *EventEmitter) HasListener(event string) bool {
	if em.handlers[event] != nil && len(em.handlers[event]) > 0 {
		return true
	}
	return false
}

/**********************************/
/*              UTILS             */
/**********************************/

// IsCallable returns true if the value can
// be called like a function or a method.
func IsCallable(value interface{}) bool {
	if reflect.ValueOf(value).Kind() == reflect.Ptr {
		return reflect.ValueOf(value).Elem().Kind() == reflect.Func
	}
	return reflect.ValueOf(value).Kind() == reflect.Func
}

// MustBeCallable is the "panicable" version of IsCallable
//
// Can Panic!
func MustBeCallable(potentialFunction interface{}) {
	if !IsCallable(potentialFunction) {
		panic(fmt.Sprintf("%+v must be callable", potentialFunction))
	}
}

// Must will panic if err is not nil
func Must(err error) {
	if err != nil {
		panic(err)
	}
}

// MustWithResult returns a result or panics on error
func MustWithResult(result interface{}, err error) interface{} {
	if err != nil {
		panic(err)
	}
	return result
}

/**********************************/
/*             TYPEDEFS           */
/**********************************/

// ResponseWriterWithCode exposes the status of a response.
type ResponseWriterWithCode struct {
	http.ResponseWriter
	code          int
	writtenLength int
}

// WriteHeader sends an HTTP response header with status code.
func (r *ResponseWriterWithCode) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Write writes to the response
func (r *ResponseWriterWithCode) Write(b []byte) (int, error) {
	i, err := r.ResponseWriter.Write(b)
	r.writtenLength = r.writtenLength + len(b)
	return i, err
}

// Code returns the response status code
func (r *ResponseWriterWithCode) Code() int {
	return r.code
}

// Length returns the number of bytes written in the response
func (r *ResponseWriterWithCode) Length() int {
	return r.writtenLength
}

// Next represents a function
type Next func()

/**********************************/
/*             MATCHERS           */
/**********************************/

// MethodMatcher matches a request by method
type MethodMatcher struct {
	methods []string
}

// NewMethodMatcher returns a new MethodMatcher
func NewMethodMatcher(verbs ...string) *MethodMatcher {
	return &MethodMatcher{methods: verbs}
}

// Match returns true if the matcher matches the request method
func (methodMatcher MethodMatcher) Match(request *http.Request) bool {
	if len(methodMatcher.methods) == 0 {
		return true
	}
	match := false
	for _, method := range methodMatcher.methods {
		if strings.ToUpper(method) == strings.ToUpper(request.Method) {
			match = true
			break
		}
	}
	return match
}

// PatternMatcher matches a request by path
type PatternMatcher struct {
	pattern *regexp.Regexp
}

// NewPatternMatcher returns a new PatternMatcher
func NewPatternMatcher(pattern *regexp.Regexp) *PatternMatcher {
	return &PatternMatcher{pattern}
}
func (patternMatcher PatternMatcher) Pattern() *regexp.Regexp {
	return patternMatcher.pattern
}

// Match returns true if the matcher matches the request url path
func (patternMatcher PatternMatcher) Match(request *http.Request) bool {
	return patternMatcher.pattern.MatchString(request.URL.Path)
}

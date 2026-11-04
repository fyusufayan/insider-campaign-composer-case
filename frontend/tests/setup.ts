// jsdom does not implement scrolling; the router's scrollBehavior calls it.
window.scrollTo = () => {}

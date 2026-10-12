Action()
{
	lr_start_transaction("type_text");
	ctrx_wait_for_event("LOGON", CTRX_LAST);
	ctrx_type("hello from loadmeter", "", CTRX_LAST);
	ctrx_key("ENTER_KEY", 0, "", CTRX_LAST);
	lr_end_transaction("type_text", LR_AUTO);
	return 0;
}

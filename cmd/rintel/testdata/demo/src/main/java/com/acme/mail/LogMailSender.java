package com.acme.mail;

import org.springframework.stereotype.Component;

@Component
public class LogMailSender implements MailSender {
    @Override
    public void send(String to, String body) {
    }
}
